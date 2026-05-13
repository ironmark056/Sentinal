package audit

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendAndRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.db")

	l, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	l.Append(Event{
		TS:        time.Now(),
		SessionID: "sess-1",
		Upstream:  "echo",
		Direction: ClientToServer,
		MsgType:   "request",
		MsgID:     "1",
		Method:    "ping",
		Payload:   json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"ping"}`),
		Bytes:     40,
	})
	l.Append(Event{
		TS:        time.Now(),
		SessionID: "sess-1",
		Upstream:  "echo",
		Direction: ServerToClient,
		MsgType:   "response",
		MsgID:     "1",
		Payload:   json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`),
		Bytes:     36,
	})

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open read-only and verify rows
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id = ?", "sess-1").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("want 2 rows, got %d", count)
	}

	var direction, method string
	err = db.QueryRow("SELECT direction, COALESCE(method, '') FROM messages WHERE session_id = ? ORDER BY id LIMIT 1", "sess-1").
		Scan(&direction, &method)
	if err != nil {
		t.Fatal(err)
	}
	if direction != string(ClientToServer) {
		t.Errorf("direction: %q", direction)
	}
	if method != "ping" {
		t.Errorf("method: %q", method)
	}
}

func TestDefaultPathOSSpecific(t *testing.T) {
	p, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if p == "" {
		t.Fatal("empty default path")
	}
}
