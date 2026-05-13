// Package dashboard serves a read-only local web UI over the audit database.
//
// Slice 5 scope: a single-page app at http://localhost:7842 that lets the
// user see what their AI agents actually did — tool calls, decisions,
// findings, payloads. Pure observation; no writes, no approvals (those land
// with slice 4 + later). Designed to outlive any single proxy invocation:
// it reads the shared SQLite audit DB and works whether the proxy is
// currently running or not.
package dashboard

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ironmark056/sentinel/internal/approval"
)

//go:embed static/*
var staticFS embed.FS

// DefaultAddr is the local-only listen address for the dashboard.
const DefaultAddr = "127.0.0.1:7842"

// Options configures a Server.
type Options struct {
	Addr      string       // listen address; defaults to DefaultAddr
	AuditPath string       // path to audit.db; "" → resolved from OS config dir
	Logger    *log.Logger  // diagnostic log; nil → stderr
}

// Server is one running dashboard instance.
type Server struct {
	opts      Options
	db        *sql.DB         // read-only handle for /api/stats and /api/events
	approvals *approval.Store // read-write handle for approval CRUD
	srv       *http.Server
	logger    *log.Logger
}

// New constructs the dashboard. Run starts serving.
func New(opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = DefaultAddr
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "[dashboard] ", log.LstdFlags|log.Lmicroseconds)
	}

	if opts.AuditPath == "" {
		p, err := defaultAuditPath()
		if err != nil {
			return nil, fmt.Errorf("resolve audit path: %w", err)
		}
		opts.AuditPath = p
	}

	if _, err := os.Stat(opts.AuditPath); err != nil {
		return nil, fmt.Errorf(
			"audit DB not found at %s — run a session via 'sentinel run' first to create one",
			opts.AuditPath)
	}

	// Open read-only — the dashboard never writes.
	db, err := sql.Open("sqlite", "file:"+opts.AuditPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open audit db: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping audit db: %w", err)
	}

	// Open a separate read-write connection for approval CRUD. SQLite WAL
	// mode tolerates one read-only and one read-write connection to the
	// same file concurrently.
	store, err := approval.Open(opts.AuditPath)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open approval store: %w", err)
	}

	s := &Server{opts: opts, db: db, approvals: store, logger: opts.Logger}
	mux := http.NewServeMux()
	s.routes(mux)
	s.srv = &http.Server{
		Addr:              opts.Addr,
		Handler:           withLogging(s.logger, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// Run starts the HTTP server and blocks until ctx is cancelled or the server
// returns an error.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.opts.Addr, err)
	}
	s.logger.Printf("dashboard listening on http://%s  (audit: %s)", ln.Addr(), s.opts.AuditPath)

	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		_ = s.Close()
		return ctx.Err()
	case err := <-errCh:
		_ = s.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Addr returns the resolved listen address. Useful after the server is
// running, especially when Opts.Addr was set to a port-zero ":0" for tests.
func (s *Server) Addr() string { return s.srv.Addr }

// Close releases both DB handles. Run also closes them on shutdown; this
// is for tests that mount routes via httptest.NewServer and bypass Run.
func (s *Server) Close() error {
	var firstErr error
	if s.db != nil {
		if err := s.db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.db = nil
	}
	if s.approvals != nil {
		if err := s.approvals.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.approvals = nil
	}
	return firstErr
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/event/", s.handleEventDetail)
	mux.HandleFunc("/api/approvals", s.handleApprovals)
	mux.HandleFunc("/api/approvals/", s.handleApprovalAction)
	mux.HandleFunc("/api/auto-decisions", s.handleAutoDecisionsList)
	mux.HandleFunc("/api/auto-decisions/", s.handleAutoDecisionDelete)

	// Serve the embedded SPA. We strip the "static/" prefix so URLs are clean:
	// GET /         → static/index.html
	// GET /app.js   → static/app.js
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		s.logger.Fatalf("embed static: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
}

func withLogging(logger *log.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(ww, r)
		if r.URL.Path != "/" && !isStaticAsset(r.URL.Path) {
			logger.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, ww.status, time.Since(start))
		}
	})
}

func isStaticAsset(p string) bool {
	switch filepath.Ext(p) {
	case ".js", ".css", ".html", ".ico", ".svg", ".png":
		return true
	}
	return false
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(c int) {
	sw.status = c
	sw.ResponseWriter.WriteHeader(c)
}

// defaultAuditPath mirrors the resolution in internal/audit.DefaultPath().
// Duplicated here to avoid importing internal/audit (no need for the writer
// half of that package on this read path).
func defaultAuditPath() (string, error) {
	if runtime.GOOS == "windows" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "sentinel", "audit.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".sentinel", "audit.db"), nil
}
