package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPUpstream_OneShotJSON(t *testing.T) {
	gotSessionEcho := int32(0)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("want POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type: %q", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Accept") == "" {
			t.Errorf("Accept header missing")
		}
		if r.Header.Get("Mcp-Session-Id") == "abc" {
			atomic.StoreInt32(&gotSessionEcho, 1)
		}
		w.Header().Set("Mcp-Session-Id", "abc")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer ts.Close()

	up, err := dialHTTPUpstream(HTTPUpstreamOptions{
		Name: "test",
		URL:  ts.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()

	if err := up.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, msg, err := up.NextFrame(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Classify() != TypeResponse {
		t.Errorf("want response, got %q", msg.Classify())
	}
	if msg.IDString() != "1" {
		t.Errorf("id: %q", msg.IDString())
	}

	// Second request should carry the session id.
	if err := up.Send([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)); err != nil {
		t.Fatal(err)
	}
	_, _, _ = up.NextFrame(ctx)
	if atomic.LoadInt32(&gotSessionEcho) != 1 {
		t.Error("expected second request to echo Mcp-Session-Id")
	}
}

func TestHTTPUpstream_SSEStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Two events, then close.
		_, _ = io.WriteString(w, `data: {"jsonrpc":"2.0","id":1,"result":{"step":1}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, `data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"value":50}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer ts.Close()

	up, err := dialHTTPUpstream(HTTPUpstreamOptions{Name: "test", URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()

	if err := up.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, m1, err := up.NextFrame(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Classify() != TypeResponse {
		t.Errorf("first frame: want response, got %q", m1.Classify())
	}
	_, m2, err := up.NextFrame(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Classify() != TypeNotification {
		t.Errorf("second frame: want notification, got %q", m2.Classify())
	}
}

func TestHTTPUpstream_SSEMultilineData(t *testing.T) {
	// Pretty-printed JSON spread across multiple data: lines should join.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\n")
		_, _ = io.WriteString(w, `data: "jsonrpc":"2.0","id":1,"result":{}`+"\n")
		_, _ = io.WriteString(w, "data: }\n\n")
	}))
	defer ts.Close()

	up, _ := dialHTTPUpstream(HTTPUpstreamOptions{Name: "test", URL: ts.URL})
	defer up.Close()

	_ = up.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"x"}`))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, msg, err := up.NextFrame(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Classify() != TypeResponse {
		t.Errorf("classify: %q", msg.Classify())
	}
}

func TestHTTPUpstream_NotificationAck(t *testing.T) {
	// Server returns 202 for notifications; we should not deliver anything.
	hit := int32(0)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hit, 1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	up, _ := dialHTTPUpstream(HTTPUpstreamOptions{Name: "test", URL: ts.URL})
	defer up.Close()

	_ = up.Send([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))

	// Wait briefly then check that no frame arrived.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, err := up.NextFrame(ctx)
	if err == nil {
		t.Error("expected no frame from 202 notification")
	}
	if atomic.LoadInt32(&hit) != 1 {
		t.Errorf("expected exactly 1 POST, got %d", hit)
	}
}

func TestHTTPUpstream_HeadersForwarded(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer ts.Close()

	up, _ := dialHTTPUpstream(HTTPUpstreamOptions{
		Name:    "test",
		URL:     ts.URL,
		Headers: map[string]string{"Authorization": "Bearer secret"},
	})
	defer up.Close()

	_ = up.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"x"}`))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _ = up.NextFrame(ctx)
	if got != "Bearer secret" {
		t.Errorf("Authorization header not forwarded: %q", got)
	}
}

func TestHTTPUpstream_BadStatusLogged(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer ts.Close()

	up, _ := dialHTTPUpstream(HTTPUpstreamOptions{Name: "test", URL: ts.URL})
	defer up.Close()

	_ = up.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"x"}`))

	// No frame should arrive.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, err := up.NextFrame(ctx)
	if err == nil {
		t.Error("expected no frame on 502")
	}
}

func TestHTTPUpstream_RejectsEmptyURL(t *testing.T) {
	_, err := dialHTTPUpstream(HTTPUpstreamOptions{Name: "test"})
	if err == nil {
		t.Fatal("expected error on empty url")
	}
}

// Ensure the upstream is fully drained on Close.
func TestHTTPUpstream_CloseDoesNotHang(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer ts.Close()

	up, _ := dialHTTPUpstream(HTTPUpstreamOptions{Name: "test", URL: ts.URL})
	_ = up.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"x"}`))
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, _, _ = up.NextFrame(ctx)

	done := make(chan struct{})
	go func() { _ = up.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
}

// Sanity: a request that produces unexpected content-type is ignored
// (doesn't crash, doesn't deliver garbage).
func TestHTTPUpstream_UnexpectedContentType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "not json")
	}))
	defer ts.Close()

	up, _ := dialHTTPUpstream(HTTPUpstreamOptions{Name: "test", URL: ts.URL})
	defer up.Close()
	_ = up.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"x"}`))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, err := up.NextFrame(ctx)
	if err == nil {
		t.Error("expected no frame on unexpected content-type")
	}
}

// One more check — the json.RawMessage payload survives the round trip.
func TestHTTPUpstream_PayloadEcho(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		var env struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &env)
		resp := `{"jsonrpc":"2.0","id":` + string(env.ID) + `,"result":{"echoed":true}}`
		_, _ = io.WriteString(w, resp)
	}))
	defer ts.Close()

	up, _ := dialHTTPUpstream(HTTPUpstreamOptions{Name: "test", URL: ts.URL})
	defer up.Close()
	_ = up.Send([]byte(`{"jsonrpc":"2.0","id":42,"method":"x"}`))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, _, err := up.NextFrame(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"id":42`) {
		t.Errorf("id round trip lost: %s", raw)
	}
}
