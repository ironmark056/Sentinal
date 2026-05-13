package proxy_test

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ironmark056/sentinel/internal/approval"
	"github.com/ironmark056/sentinel/internal/audit"
	"github.com/ironmark056/sentinel/internal/policy"
	"github.com/ironmark056/sentinel/internal/proxy"
)

// TestProxy_BlocksDangerousCommand pins slice 4 block behavior: a critical
// finding (e.g. "rm -rf /") drives the risk score above the block threshold,
// the call never reaches the upstream, and the client gets a JSON-RPC error.
func TestProxy_BlocksDangerousCommand(t *testing.T) {
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
		Upstream:  proxy.Upstream{Name: "echo", Command: echomcpBin},
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
		var parsed struct {
			JSONRPC string `json:"jsonrpc"`
			ID      int    `json:"id"`
			Error   *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(resp)), &parsed); err != nil {
			t.Fatalf("invalid response JSON: %v\nresponse: %s", err, resp)
		}
		if parsed.Error == nil {
			t.Fatalf("expected an error response, got: %s", resp)
		}
		if parsed.Error.Code != -32000 {
			t.Errorf("expected code -32000, got %d", parsed.Error.Code)
		}
		if !strings.Contains(parsed.Error.Message, "blocked by sentinel policy") {
			t.Errorf("expected block message, got %q", parsed.Error.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for block response")
	}

	_ = clientInW.Close()
	cancel()
	_ = clientOutW.Close()
	_ = clientOutR.Close()
	_ = clientInR.Close()

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Error("proxy did not shut down")
	}
}

// TestProxy_ApprovalApproved tests the happy path of the approval flow:
// SSH path access scores 50 → DecisionApprove → proxy inserts a pending row
// and waits. The test (acting as the dashboard) approves it. The proxy
// should then forward the request to the upstream, which echoes a response.
func TestProxy_ApprovalApproved(t *testing.T) {
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
		Upstream:        proxy.Upstream{Name: "echo", Command: echomcpBin},
		Audit:           au,
		Policy:          policy.NewEngine(nil),
		Approvals:       store,
		ApprovalTimeout: 5 * time.Second,
		ClientIn:        clientInR,
		ClientOut:       clientOutW,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = p.Run(ctx); close(runDone) }()

	req := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"filesystem.read","arguments":{"path":"/home/alice/.ssh/id_rsa"}}}` + "\n"
	_, _ = clientInW.Write([]byte(req))

	// Wait for the proxy to insert a pending approval row, then approve it.
	approveDone := make(chan struct{})
	go func() {
		defer close(approveDone)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			pending, err := store.ListPending(context.Background(), 10)
			if err != nil {
				t.Logf("list pending: %v", err)
				return
			}
			if len(pending) > 0 {
				if err := store.Resolve(context.Background(), pending[0].ID, approval.StatusApproved, "test"); err != nil {
					t.Errorf("resolve: %v", err)
				}
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Error("never saw a pending approval row")
	}()

	respCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := clientOutR.Read(buf)
		respCh <- string(buf[:n])
	}()

	select {
	case resp := <-respCh:
		if strings.Contains(resp, `"error"`) {
			t.Errorf("approved call should not produce error, got: %s", resp)
		}
		if !strings.Contains(resp, `"result"`) {
			t.Errorf("expected result, got: %s", resp)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for response after approval")
	}
	<-approveDone

	_ = clientInW.Close()
	cancel()
	_ = clientOutW.Close()
	_ = clientOutR.Close()
	_ = clientInR.Close()
	<-runDone
}

// TestProxy_ApprovalDenied — same flow, but the human denies. The proxy
// must return the JSON-RPC error to the client and never forward.
func TestProxy_ApprovalDenied(t *testing.T) {
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
		Upstream:        proxy.Upstream{Name: "echo", Command: echomcpBin},
		Audit:           au,
		Policy:          policy.NewEngine(nil),
		Approvals:       store,
		ApprovalTimeout: 5 * time.Second,
		ClientIn:        clientInR,
		ClientOut:       clientOutW,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = p.Run(ctx); close(runDone) }()

	req := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"filesystem.read","arguments":{"path":"/home/alice/.ssh/id_rsa"}}}` + "\n"
	_, _ = clientInW.Write([]byte(req))

	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			pending, _ := store.ListPending(context.Background(), 10)
			if len(pending) > 0 {
				_ = store.Resolve(context.Background(), pending[0].ID, approval.StatusDenied, "test")
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	respCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := clientOutR.Read(buf)
		respCh <- string(buf[:n])
	}()

	select {
	case resp := <-respCh:
		if !strings.Contains(resp, `"error"`) {
			t.Errorf("denied call should produce error, got: %s", resp)
		}
		if !strings.Contains(resp, "denied by user") {
			t.Errorf("error should mention 'denied by user', got: %s", resp)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for response after denial")
	}

	_ = clientInW.Close()
	cancel()
	_ = clientOutW.Close()
	_ = clientOutR.Close()
	_ = clientInR.Close()
	<-runDone
}

// TestProxy_AllowsBenignCall — the negative: a benign call still passes
// through and reaches the upstream.
func TestProxy_AllowsBenignCall(t *testing.T) {
	tmp := t.TempDir()
	au, err := audit.New(filepath.Join(tmp, "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer au.Close()

	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()

	p, err := proxy.New(proxy.Options{
		Upstream:  proxy.Upstream{Name: "echo", Command: echomcpBin},
		Audit:     au,
		Policy:    policy.NewEngine(nil),
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

	req := `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"list_repos","arguments":{"owner":"octocat"}}}` + "\n"
	_, _ = clientInW.Write([]byte(req))

	respCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := clientOutR.Read(buf)
		respCh <- string(buf[:n])
	}()

	select {
	case resp := <-respCh:
		// echomcp echoes the method back as result. We expect a "result",
		// not an error.
		if strings.Contains(resp, `"error"`) {
			t.Errorf("benign call incorrectly blocked: %s", resp)
		}
		if !strings.Contains(resp, `"result"`) {
			t.Errorf("expected result, got: %s", resp)
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
}
