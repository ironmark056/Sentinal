package proxy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ironmark056/sentinel/internal/approval"
	"github.com/ironmark056/sentinel/internal/audit"
	"github.com/ironmark056/sentinel/internal/policy"
	"github.com/ironmark056/sentinel/internal/proxy"
)

// httpMCPHandler responds to JSON-RPC POSTs by echoing the method back.
// Mirrors what testdata/echomcp does over stdio.
func httpMCPHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var env struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &env)
		if len(env.ID) == 0 || string(env.ID) == "null" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(env.ID),
			"result":  map[string]any{"echo": env.Method, "transport": "http"},
		}
		out, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}
}

// TestProxy_HTTPUpstream_RoundTrip pins the end-to-end path with an HTTP
// upstream: client → proxy → POST to httptest server → response → client,
// with the audit DB picking up both ends.
func TestProxy_HTTPUpstream_RoundTrip(t *testing.T) {
	srv := httptest.NewServer(httpMCPHandler(t))
	defer srv.Close()

	tmp := t.TempDir()
	auditPath := filepath.Join(tmp, "audit.db")
	au, err := audit.New(auditPath)
	if err != nil {
		t.Fatal(err)
	}

	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()

	p, err := proxy.New(proxy.Options{
		Upstream: proxy.Upstream{
			Name: "remote",
			URL:  srv.URL,
		},
		Audit:     au,
		ClientIn:  clientInR,
		ClientOut: clientOutW,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = p.Run(ctx); close(runDone) }()

	req := `{"jsonrpc":"2.0","id":7,"method":"tools/list"}` + "\n"
	_, _ = clientInW.Write([]byte(req))

	respCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := clientOutR.Read(buf)
		respCh <- string(buf[:n])
	}()

	select {
	case resp := <-respCh:
		if !strings.Contains(resp, `"id":7`) {
			t.Errorf("id round-trip lost: %s", resp)
		}
		if !strings.Contains(resp, `"transport":"http"`) {
			t.Errorf("expected upstream echo to mention http transport, got: %s", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}

	_ = clientInW.Close()
	cancel()
	_ = clientOutW.Close()
	_ = clientOutR.Close()
	_ = clientInR.Close()
	<-runDone

	_ = au.Close()
}

// Same flow as above, but with a policy engine in the path: blocked calls
// should still be blocked when the upstream is HTTP — proves the policy
// pipeline is transport-agnostic.
func TestProxy_HTTPUpstream_BlocksDangerousCommand(t *testing.T) {
	srv := httptest.NewServer(httpMCPHandler(t))
	defer srv.Close()

	tmp := t.TempDir()
	auditPath := filepath.Join(tmp, "audit.db")
	au, err := audit.New(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	defer au.Close()
	store, err := approval.Open(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()

	p, err := proxy.New(proxy.Options{
		Upstream:  proxy.Upstream{Name: "remote", URL: srv.URL},
		Audit:     au,
		Policy:    policy.NewEngine(nil),
		Approvals: store,
		ClientIn:  clientInR,
		ClientOut: clientOutW,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = p.Run(ctx); close(runDone) }()

	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shell.exec","arguments":{"command":"rm -rf /tmp"}}}` + "\n"
	_, _ = clientInW.Write([]byte(req))

	respCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := clientOutR.Read(buf)
		respCh <- string(buf[:n])
	}()

	select {
	case resp := <-respCh:
		if !strings.Contains(resp, `"error"`) || !strings.Contains(resp, "blocked by sentinel policy") {
			t.Errorf("expected policy block on HTTP upstream, got: %s", resp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out")
	}

	_ = clientInW.Close()
	cancel()
	_ = clientOutW.Close()
	_ = clientOutR.Close()
	_ = clientInR.Close()
	<-runDone
}
