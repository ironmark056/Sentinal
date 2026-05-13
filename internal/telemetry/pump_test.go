package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironmark056/sentinel/internal/audit"
)

// receivedBatches captures every batch the test server received.
type receivedBatches struct {
	mu      sync.Mutex
	batches [][]ingestEvent
	calls   int32
	want401 bool
}

func (r *receivedBatches) all() [][]ingestEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]ingestEvent, len(r.batches))
	copy(out, r.batches)
	return out
}

func (r *receivedBatches) handler(expectedToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&r.calls, 1)
		if r.want401 || req.Header.Get("Authorization") != "Bearer "+expectedToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if req.URL.Path == "/agent/v1/health" {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		var body ingestBody
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.batches = append(r.batches, body.Events)
		r.mu.Unlock()
		_, _ = io.WriteString(w, `{"accepted":`+itoa(int64(len(body.Events)))+`}`)
	}
}

func seedAudit(t *testing.T, n int) (*audit.Log, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.db")
	l, err := audit.New(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		l.Append(audit.Event{
			TS:        time.Now(),
			SessionID: "sess",
			Upstream:  "echo",
			Direction: audit.ClientToServer,
			MsgType:   "request",
			MsgID:     itoa(int64(i + 1)),
			Method:    "tools/list",
			Payload:   []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
			Bytes:     44,
		})
	}
	// Force flush.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen for use.
	l, err = audit.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path
}

func TestPump_ShipsEventsAndAdvancesCursor(t *testing.T) {
	rb := &receivedBatches{}
	const token = "mcpg_test"
	ts := httptest.NewServer(rb.handler(token))
	defer ts.Close()

	l, _ := seedAudit(t, 5)
	pump, err := New(Options{
		URL:       ts.URL,
		Token:     token,
		AgentName: "test",
		Audit:     l,
		Interval:  50 * time.Millisecond,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { _ = pump.Run(ctx); close(done) }()

	// Wait until all 5 events have been shipped.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		all := rb.all()
		count := 0
		for _, b := range all {
			count += len(b)
		}
		if count >= 5 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	all := rb.all()
	total := 0
	for _, b := range all {
		total += len(b)
	}
	if total < 5 {
		t.Errorf("want >=5 events shipped, got %d (batches=%d)", total, len(all))
	}

	// Cursor should reflect the last shipped id.
	cur, err := l.GetCursor(context.Background(), "central:"+ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if cur == "" {
		t.Error("cursor not persisted")
	}
}

func TestPump_ResumesFromCursorAcrossRestarts(t *testing.T) {
	rb := &receivedBatches{}
	const token = "mcpg_test"
	ts := httptest.NewServer(rb.handler(token))
	defer ts.Close()

	l, _ := seedAudit(t, 3)
	// First run pushes 3 events.
	p1, _ := New(Options{URL: ts.URL, Token: token, AgentName: "x", Audit: l,
		Interval: 50 * time.Millisecond, BatchSize: 10})
	ctx1, cancel1 := context.WithTimeout(context.Background(), 1*time.Second)
	go func() { _ = p1.Run(ctx1) }()

	// Wait for the 3 events.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		all := rb.all()
		c := 0
		for _, b := range all {
			c += len(b)
		}
		if c >= 3 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	cancel1()

	// Append two more events post-cursor.
	for i := 0; i < 2; i++ {
		l.Append(audit.Event{
			TS:        time.Now(),
			SessionID: "sess",
			Upstream:  "echo",
			Direction: audit.ClientToServer,
			MsgType:   "request",
			Payload:   []byte(`{}`),
			Bytes:     2,
		})
	}
	time.Sleep(100 * time.Millisecond) // let audit flush

	// Second pump should ship only the 2 new ones.
	rb2 := &receivedBatches{}
	ts2 := httptest.NewServer(rb2.handler(token))
	defer ts2.Close()
	// Use the SAME URL so the cursor matches.
	rb3 := &receivedBatches{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", rb3.handler(token))
	_ = mux // not used; we just reuse rb

	p2, _ := New(Options{URL: ts.URL, Token: token, AgentName: "x", Audit: l,
		Interval: 50 * time.Millisecond, BatchSize: 10})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1*time.Second)
	go func() { _ = p2.Run(ctx2) }()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		all := rb.all()
		c := 0
		for _, b := range all {
			c += len(b)
		}
		if c >= 5 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	cancel2()

	total := 0
	for _, b := range rb.all() {
		total += len(b)
	}
	if total != 5 {
		t.Errorf("after resume: want exactly 5 events total, got %d (duplicates would push over 5)", total)
	}
}

func TestPump_SurvivesServerErrors(t *testing.T) {
	rb := &receivedBatches{want401: true}
	const token = "mcpg_test"
	ts := httptest.NewServer(rb.handler(token))
	defer ts.Close()

	l, _ := seedAudit(t, 2)
	p, _ := New(Options{URL: ts.URL, Token: token, AgentName: "x", Audit: l,
		Interval: 30 * time.Millisecond, BatchSize: 10})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	<-done

	// Server kept returning 401; cursor must not advance.
	cur, _ := l.GetCursor(context.Background(), "central:"+ts.URL)
	if cur != "" && cur != "0" {
		t.Errorf("cursor should not advance on failure, got %q", cur)
	}
}

func TestPump_RejectsBadOptions(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("empty options should error")
	}
	if _, err := New(Options{URL: "http://x"}); err == nil {
		t.Error("missing token should error")
	}
	if _, err := New(Options{URL: "http://x", Token: "t"}); err == nil {
		t.Error("missing audit should error")
	}
}

func TestPump_CheckHealth(t *testing.T) {
	rb := &receivedBatches{}
	const token = "mcpg_test"
	ts := httptest.NewServer(rb.handler(token))
	defer ts.Close()

	l, _ := seedAudit(t, 0)
	p, _ := New(Options{URL: ts.URL, Token: token, AgentName: "x", Audit: l,
		Interval: time.Second, BatchSize: 10})

	if err := p.CheckHealth(context.Background()); err != nil {
		t.Errorf("health check failed: %v", err)
	}

	// Bad token.
	p2, _ := New(Options{URL: ts.URL, Token: "wrong", AgentName: "x", Audit: l,
		Interval: time.Second})
	if err := p2.CheckHealth(context.Background()); err == nil {
		t.Error("bad token should fail health check")
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
