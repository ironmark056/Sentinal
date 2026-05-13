package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

//go:embed static/*
var staticFS embed.FS

// DefaultAddr is the listen address the server uses if none is configured.
const DefaultAddr = "0.0.0.0:7843"

// Options configures a Server.
type Options struct {
	Addr    string // listen address; defaults to DefaultAddr
	DataDir string // SQLite + state lives here; defaults to ./data
	Logger  *log.Logger
	// AdminToken, if non-empty, gates write endpoints on the dashboard
	// (creating agents). When empty, the dashboard is fully open — fine
	// for a single-operator deployment behind a VPN, dangerous otherwise.
	AdminToken string
}

// Server is one running sentinel-server instance.
type Server struct {
	opts    Options
	store   *Storage
	srv     *http.Server
	logger  *log.Logger
}

// New constructs the server but does not start listening.
func New(opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = DefaultAddr
	}
	if opts.DataDir == "" {
		opts.DataDir = "./data"
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "[sentinel-server] ", log.LstdFlags|log.Lmicroseconds)
	}

	abs, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}
	opts.DataDir = abs

	store, err := Open(opts.DataDir)
	if err != nil {
		return nil, err
	}

	s := &Server{opts: opts, store: store, logger: opts.Logger}
	mux := http.NewServeMux()
	s.routes(mux)
	s.srv = &http.Server{
		Addr:              opts.Addr,
		Handler:           s.withMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// Run starts serving and blocks until ctx is cancelled or the server errors.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.opts.Addr, err)
	}
	s.logger.Printf("sentinel-server listening on %s (data: %s)", ln.Addr(), s.opts.DataDir)
	if s.opts.AdminToken == "" {
		s.logger.Printf("WARNING: ADMIN_TOKEN unset — dashboard write endpoints are open. Set SENTINEL_ADMIN_TOKEN in production.")
	}

	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		_ = s.store.Close()
		return ctx.Err()
	case err := <-errCh:
		_ = s.store.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Close releases resources. Used by tests; production uses Run's defer.
func (s *Server) Close() error {
	if s.store != nil {
		_ = s.store.Close()
		s.store = nil
	}
	return nil
}

// Storage returns the underlying store, primarily for tests.
func (s *Server) Storage() *Storage { return s.store }

func (s *Server) routes(mux *http.ServeMux) {
	// Health / version (open).
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/version", s.handleVersion)

	// Agent API — bearer-token auth required.
	mux.HandleFunc("/agent/v1/events", s.requireAgent(s.handleAgentEvents))
	mux.HandleFunc("/agent/v1/health", s.requireAgent(s.handleAgentHealth))
	mux.HandleFunc("/agent/v1/policy", s.requireAgent(s.handleAgentPolicy))

	// Dashboard API — admin-token gated when AdminToken is set.
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/agents", s.handleAgents)
	mux.HandleFunc("/api/agents/", s.requireAdmin(s.handleAgentMutate))
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/events/", s.handleEventDetail)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/enroll", s.handleEnroll)
	mux.HandleFunc("/api/enroll/", s.requireAdmin(s.handleEnrollMutate))
	mux.HandleFunc("/api/policy", s.handlePolicy)
	mux.HandleFunc("/api/policy/revisions", s.handlePolicyRevisions)

	// Public, no auth — the OTT in the path is the credential.
	mux.HandleFunc("/e/", s.handleEnrollConsume)

	// Embedded SPA.
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		s.logger.Fatalf("embed static: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

func (s *Server) withMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(ww, r)
		// Don't log every static asset; just API + agent calls.
		p := r.URL.Path
		if p == "/" || hasStaticExt(p) {
			return
		}
		s.logger.Printf("%s %s -> %d (%s)", r.Method, p, ww.status, time.Since(start))
	})
}

func hasStaticExt(p string) bool {
	switch filepath.Ext(p) {
	case ".js", ".css", ".html", ".ico", ".svg", ".png", ".woff", ".woff2":
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
