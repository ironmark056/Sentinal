package proxy_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ironmark056/sentinel/internal/audit"
	"github.com/ironmark056/sentinel/internal/proxy"
)

// echomcpBin is set by TestMain to the path of a freshly built echomcp.
var echomcpBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sentinel-int-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	bin := filepath.Join(dir, "echomcp")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	// Find the repo root by walking up from CWD until we find go.mod.
	root := findRepoRoot()
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/echomcp")
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("build echomcp: " + err.Error())
	}
	echomcpBin = bin

	os.Exit(m.Run())
}

func findRepoRoot() string {
	d, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			panic("could not find go.mod")
		}
		d = parent
	}
}

// TestProxy_RoundTrip starts a real upstream subprocess and verifies that
// a request sent to the proxy reaches the upstream and a response comes back.
func TestProxy_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	auditPath := filepath.Join(tmp, "audit.db")
	au, err := audit.New(auditPath)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}

	// Pipe pairs: client writes to clientIn (proxy reads); proxy writes to
	// clientOut (we read).
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()

	p, err := proxy.New(proxy.Options{
		Upstream: proxy.Upstream{
			Name:    "echo",
			Command: echomcpBin,
		},
		Audit:     au,
		ClientIn:  clientInR,
		ClientOut: clientOutW,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() {
		runDone <- p.Run(ctx)
	}()

	// Send a request.
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	if _, err := clientInW.Write([]byte(req)); err != nil {
		t.Fatalf("write req: %v", err)
	}

	// Read response with timeout.
	respCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := clientOutR.Read(buf)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- string(buf[:n])
	}()

	select {
	case resp := <-respCh:
		if !strings.Contains(resp, `"id":1`) || !strings.Contains(resp, `"result"`) {
			t.Errorf("unexpected response: %q", resp)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(resp)), &parsed); err != nil {
			t.Errorf("response not valid JSON: %v", err)
		}
	case err := <-errCh:
		t.Fatalf("read response: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for response")
	}

	// Shut everything down cleanly.
	_ = clientInW.Close()
	cancel()
	_ = clientOutW.Close()
	_ = clientOutR.Close()
	_ = clientInR.Close()

	select {
	case err := <-runDone:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.ErrClosedPipe) {
			t.Logf("proxy run returned: %v (acceptable on shutdown)", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("proxy did not shut down in time")
	}

	if err := au.Close(); err != nil {
		t.Fatalf("audit close: %v", err)
	}

	// Inspect audit DB.
	db, err := sql.Open("sqlite", auditPath)
	if err != nil {
		t.Fatalf("open audit: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count < 2 {
		t.Errorf("want at least 2 audit rows (req + resp), got %d", count)
	}

	rows, err := db.Query("SELECT direction, msg_type, COALESCE(method, '') FROM messages ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seen []string
	for rows.Next() {
		var dir, typ, method string
		if err := rows.Scan(&dir, &typ, &method); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, dir+"|"+typ+"|"+method)
	}
	t.Logf("audit rows: %v", seen)
	// Must contain at least one c2s request for tools/list and one s2c response.
	hasRequest, hasResponse := false, false
	for _, s := range seen {
		if s == "c2s|request|tools/list" {
			hasRequest = true
		}
		if strings.HasPrefix(s, "s2c|response|") {
			hasResponse = true
		}
	}
	if !hasRequest {
		t.Error("expected c2s request for tools/list in audit log")
	}
	if !hasResponse {
		t.Error("expected s2c response in audit log")
	}
}
