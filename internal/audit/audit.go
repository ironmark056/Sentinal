// Package audit persists every MCP message passing through the proxy.
//
// The audit log is the source of truth for what an AI agent has done.
// It is append-only, indexed by timestamp and session, and written via a
// buffered channel so the proxy hot path is never blocked on disk I/O.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Direction describes which way a message was flowing.
type Direction string

const (
	ClientToServer Direction = "c2s"
	ServerToClient Direction = "s2c"
)

// Event is one row in the audit log.
type Event struct {
	TS         time.Time
	SessionID  string
	Upstream   string
	Direction  Direction
	MsgType    string
	MsgID      string
	Method     string
	Payload    json.RawMessage
	Bytes      int
}

// Log is the audit log writer. Use New to construct, Append to enqueue
// events, and Close to flush and shut down.
type Log struct {
	db     *sql.DB
	in     chan Event
	wg     sync.WaitGroup
	closed chan struct{}
}

// DefaultPath returns the OS-appropriate default audit DB path.
//   macOS/Linux: ~/.sentinel/audit.db
//   Windows:     %APPDATA%\sentinel\audit.db
func DefaultPath() (string, error) {
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

// New opens (or creates) the audit DB at path and starts the writer goroutine.
// Pass an empty string to use DefaultPath.
func New(path string) (*Log, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir audit dir: %w", err)
	}

	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open audit db: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping audit db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	l := &Log{
		db:     db,
		in:     make(chan Event, 1024),
		closed: make(chan struct{}),
	}
	l.wg.Add(1)
	go l.writer()
	return l, nil
}

// Append enqueues an event. Returns immediately. If the queue is full the
// event is dropped and a warning is logged — under sustained backpressure
// it is better to lose a log line than to stall the proxy.
func (l *Log) Append(e Event) {
	select {
	case l.in <- e:
	case <-l.closed:
	default:
		log.Printf("audit: queue full, dropping event (session=%s upstream=%s method=%s)",
			e.SessionID, e.Upstream, e.Method)
	}
}

// Close flushes pending events and closes the DB.
func (l *Log) Close() error {
	close(l.closed)
	close(l.in)
	l.wg.Wait()
	return l.db.Close()
}

// Path returns the on-disk path of the audit DB (best-effort, used for logs).
func (l *Log) Path() string {
	var s string
	_ = l.db.QueryRow("PRAGMA database_list").Scan(new(int), new(string), &s)
	return s
}

// ReadEvent is the read-side shape returned by ReadEventsAfter.
type ReadEvent struct {
	ID        int64
	TS        time.Time
	SessionID string
	Upstream  string
	Direction Direction
	MsgType   string
	MsgID     string
	Method    string
	Payload   []byte
	Bytes     int
}

// ReadEventsAfter returns up to limit events with id > afterID, ordered
// asc. Used by the telemetry pump to ship events to central in batches.
func (l *Log) ReadEventsAfter(ctx context.Context, afterID int64, limit int) ([]ReadEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT id, ts, session_id, upstream, direction, msg_type,
		       COALESCE(msg_id, ''), COALESCE(method, ''), payload, bytes
		FROM messages WHERE id > ? ORDER BY id ASC LIMIT ?`,
		afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReadEvent
	for rows.Next() {
		var e ReadEvent
		var ts int64
		var dir string
		if err := rows.Scan(&e.ID, &ts, &e.SessionID, &e.Upstream, &dir,
			&e.MsgType, &e.MsgID, &e.Method, &e.Payload, &e.Bytes); err != nil {
			return nil, err
		}
		e.TS = time.Unix(0, ts)
		e.Direction = Direction(dir)
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetCursor returns the persisted value for key, or "" if not set.
func (l *Log) GetCursor(ctx context.Context, key string) (string, error) {
	var v string
	err := l.db.QueryRowContext(ctx,
		`SELECT value FROM telemetry_state WHERE key = ?`, key).Scan(&v)
	if err != nil {
		// sql.ErrNoRows is fine — return empty string.
		if err.Error() == "sql: no rows in result set" {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

// SetCursor persists the value for key, overwriting any existing value.
func (l *Log) SetCursor(ctx context.Context, key, value string) error {
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO telemetry_state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

func (l *Log) writer() {
	defer l.wg.Done()
	ctx := context.Background()
	const batchSize = 32
	const flushInterval = 500 * time.Millisecond

	batch := make([]Event, 0, batchSize)
	flushTimer := time.NewTimer(flushInterval)
	defer flushTimer.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := l.flushBatch(ctx, batch); err != nil {
			log.Printf("audit: flush failed: %v", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-l.in:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= batchSize {
				flush()
				if !flushTimer.Stop() {
					select {
					case <-flushTimer.C:
					default:
					}
				}
				flushTimer.Reset(flushInterval)
			}
		case <-flushTimer.C:
			flush()
			flushTimer.Reset(flushInterval)
		}
	}
}

func (l *Log) flushBatch(ctx context.Context, batch []Event) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range batch {
		_, err := stmt.ExecContext(ctx,
			e.TS.UnixNano(),
			e.SessionID,
			e.Upstream,
			string(e.Direction),
			e.MsgType,
			nullable(e.MsgID),
			nullable(e.Method),
			[]byte(e.Payload),
			e.Bytes,
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const schema = `
CREATE TABLE IF NOT EXISTS messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          INTEGER NOT NULL,
    session_id  TEXT NOT NULL,
    upstream    TEXT NOT NULL,
    direction   TEXT NOT NULL,
    msg_type    TEXT NOT NULL,
    msg_id      TEXT,
    method      TEXT,
    payload     BLOB NOT NULL,
    bytes       INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_ts        ON messages(ts);
CREATE INDEX IF NOT EXISTS idx_messages_session   ON messages(session_id);
CREATE INDEX IF NOT EXISTS idx_messages_method    ON messages(method);
CREATE INDEX IF NOT EXISTS idx_messages_upstream  ON messages(upstream);

CREATE TABLE IF NOT EXISTS sessions (
    id           TEXT PRIMARY KEY,
    started_at   INTEGER NOT NULL,
    ended_at     INTEGER,
    upstream     TEXT NOT NULL,
    proxy_pid    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS telemetry_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

const insertSQL = `
INSERT INTO messages (ts, session_id, upstream, direction, msg_type, msg_id, method, payload, bytes)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
`
