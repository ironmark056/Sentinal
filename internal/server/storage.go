// Package server implements the sentinel-server binary: a self-hosted
// central service that ingests audit events from a fleet of sentinel
// agents (employee laptops, automation servers) and serves a multi-agent
// dashboard over HTTP.
//
// All data lives in the customer's own infrastructure. There is no SaaS
// component; the binary is configured with a local data directory and
// listens on a port the operator picks.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Storage owns the central SQLite database.
type Storage struct {
	db *sql.DB
}

// Agent is one registered agent (one employee laptop, one automation host).
type Agent struct {
	ID       int64
	Name     string
	Created  time.Time
	LastSeen time.Time
	Metadata []byte // JSON: hostname, os, version, etc.
}

// Event is one audit row mirrored from an agent's local audit log.
type Event struct {
	ID        int64
	AgentID   int64
	AgentName string // joined from agents table for convenience
	AgentTS   time.Time
	ServerTS  time.Time
	SessionID string
	Upstream  string
	Direction string
	MsgType   string
	MsgID     string
	Method    string
	Payload   []byte
	Bytes     int
}

// Open creates or opens the central SQLite DB at dataDir/sentinel-server.db
// and applies schema. dataDir must exist or be creatable.
func Open(dataDir string) (*Storage, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir data dir: %w", err)
	}
	path := filepath.Join(dataDir, "sentinel-server.db")
	db, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Storage{db: db}, nil
}

// Close releases the DB handle.
func (s *Storage) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB exposes the underlying *sql.DB. Intended for tests; production
// callers should use the typed methods above.
func (s *Storage) DB() *sql.DB { return s.db }

// ---------------------------------------------------------------------------
// Agent management
// ---------------------------------------------------------------------------

// NewAgentToken returns a random 256-bit token formatted as
// "mcpg_<hex>". Callers use HashToken before persisting.
func NewAgentToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand reading from OS RNG should never fail; if it does,
		// crashing is the right behavior — we cannot generate safe tokens.
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return "mcpg_" + hex.EncodeToString(b[:])
}

// HashToken returns the hex SHA-256 of the token; we only persist the
// hash, never the original.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateAgent registers a new agent and returns the generated token.
// The token is shown ONCE to the admin — only its hash is stored.
func (s *Storage) CreateAgent(ctx context.Context, name string, metadata map[string]string) (string, *Agent, error) {
	if name == "" {
		return "", nil, errors.New("agent name is required")
	}
	token := NewAgentToken()
	hash := HashToken(token)

	metaJSON, _ := json.Marshal(metadata)
	now := time.Now().UnixNano()

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agents (name, token_hash, created_at, metadata) VALUES (?, ?, ?, ?)`,
		name, hash, now, metaJSON)
	if err != nil {
		return "", nil, fmt.Errorf("insert agent: %w", err)
	}
	id, _ := res.LastInsertId()
	return token, &Agent{
		ID:       id,
		Name:     name,
		Created:  time.Unix(0, now),
		Metadata: metaJSON,
	}, nil
}

// LookupAgentByToken returns the agent matching the given (raw) token, or
// nil if no match. Also updates last_seen as a side effect.
func (s *Storage) LookupAgentByToken(ctx context.Context, token string) (*Agent, error) {
	if token == "" {
		return nil, nil
	}
	hash := HashToken(token)
	var (
		a        Agent
		created  int64
		lastSeen sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, created_at, last_seen, COALESCE(metadata, '{}')
		 FROM agents WHERE token_hash = ?`, hash).
		Scan(&a.ID, &a.Name, &created, &lastSeen, &a.Metadata)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	a.Created = time.Unix(0, created)
	if lastSeen.Valid {
		a.LastSeen = time.Unix(0, lastSeen.Int64)
	}
	// Touch last_seen.
	_, _ = s.db.ExecContext(ctx,
		`UPDATE agents SET last_seen = ? WHERE id = ?`,
		time.Now().UnixNano(), a.ID)
	return &a, nil
}

// ListAgents returns all agents ordered by name.
func (s *Storage) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, created_at, COALESCE(last_seen, 0), COALESCE(metadata, '{}')
		 FROM agents ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		var created, lastSeen int64
		if err := rows.Scan(&a.ID, &a.Name, &created, &lastSeen, &a.Metadata); err != nil {
			return nil, err
		}
		a.Created = time.Unix(0, created)
		if lastSeen > 0 {
			a.LastSeen = time.Unix(0, lastSeen)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAgent removes an agent and its events. Used for revoke.
func (s *Storage) DeleteAgent(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM agents WHERE id = ?`, id)
	return err
}

// ---------------------------------------------------------------------------
// Enrollments (slice 0.2.5 — one-time onboarding tokens)
// ---------------------------------------------------------------------------

// Enrollment is one outstanding device-enrollment token. The OTT itself
// is never stored in cleartext; only its SHA-256 hash. Single-use:
// ConsumeEnrollment is atomic with respect to creating the resulting
// agent.
type Enrollment struct {
	ID         int64
	Name       string
	Created    time.Time
	Expires    time.Time
	Consumed   time.Time // zero-value if still outstanding
	Metadata   []byte    // JSON
	ResolvedID int64     // agent id this enrollment turned into (after consume)
}

// NewEnrollmentOTT returns a fresh random one-time token. Same shape as
// agent tokens but prefixed differently to make accidental misuse
// obvious in logs.
func NewEnrollmentOTT() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return "ott_" + hex.EncodeToString(b[:])
}

// CreateEnrollment records a single-use enrollment that, when consumed,
// will produce an agent under the given name. ttl controls how long the
// OTT is valid for (24h is the suggested default).
func (s *Storage) CreateEnrollment(ctx context.Context, name string, ttl time.Duration, metadata map[string]string) (string, *Enrollment, error) {
	if name == "" {
		return "", nil, errors.New("enrollment: name is required")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	ott := NewEnrollmentOTT()
	now := time.Now()
	expires := now.Add(ttl)
	metaJSON, _ := json.Marshal(metadata)

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO enrollments (name, ott_hash, created_at, expires_at, metadata)
		VALUES (?, ?, ?, ?, ?)`,
		name, HashToken(ott), now.UnixNano(), expires.UnixNano(), metaJSON)
	if err != nil {
		return "", nil, fmt.Errorf("insert enrollment: %w", err)
	}
	id, _ := res.LastInsertId()
	return ott, &Enrollment{
		ID:       id,
		Name:     name,
		Created:  now,
		Expires:  expires,
		Metadata: metaJSON,
	}, nil
}

// ConsumeEnrollment is the public-facing operation an employee triggers
// when they run `sentinel enroll`. Validates the OTT, creates an agent
// under the enrollment's name, marks the enrollment consumed — all in
// one transaction so a token can never produce two agents.
//
// Returns (bearerToken, agent, nil) on success. Errors:
//   - errEnrollmentNotFound  — unknown OTT
//   - errEnrollmentExpired   — past expires_at
//   - errEnrollmentConsumed  — already used
//   - errEnrollmentNameTaken — an agent with the same name already exists
//     (admin tried to re-register without revoking first)
func (s *Storage) ConsumeEnrollment(ctx context.Context, ott string) (string, *Agent, error) {
	hash := HashToken(ott)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		id          int64
		name        string
		expires     int64
		consumed    sql.NullInt64
		metadataRaw []byte
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, name, expires_at, consumed_at, COALESCE(metadata, '{}')
		FROM enrollments WHERE ott_hash = ?`, hash).
		Scan(&id, &name, &expires, &consumed, &metadataRaw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, ErrEnrollmentNotFound
		}
		return "", nil, err
	}
	if consumed.Valid {
		return "", nil, ErrEnrollmentConsumed
	}
	if time.Now().UnixNano() > expires {
		return "", nil, ErrEnrollmentExpired
	}

	// Create the agent.
	token := NewAgentToken()
	tokenHash := HashToken(token)
	now := time.Now().UnixNano()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO agents (name, token_hash, created_at, metadata) VALUES (?, ?, ?, ?)`,
		name, tokenHash, now, metadataRaw)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return "", nil, ErrEnrollmentNameTaken
		}
		return "", nil, fmt.Errorf("insert agent: %w", err)
	}
	agentID, _ := res.LastInsertId()

	// Mark the enrollment consumed and link to the agent it produced.
	if _, err := tx.ExecContext(ctx,
		`UPDATE enrollments SET consumed_at = ?, resolved_agent_id = ? WHERE id = ?`,
		now, agentID, id); err != nil {
		return "", nil, err
	}

	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return token, &Agent{
		ID:       agentID,
		Name:     name,
		Created:  time.Unix(0, now),
		Metadata: metadataRaw,
	}, nil
}

// ListEnrollments returns outstanding enrollments ordered by created_at desc.
func (s *Storage) ListEnrollments(ctx context.Context) ([]Enrollment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, created_at, expires_at, COALESCE(consumed_at, 0),
		       COALESCE(metadata, '{}'), COALESCE(resolved_agent_id, 0)
		FROM enrollments
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Enrollment
	for rows.Next() {
		var e Enrollment
		var created, expires, consumed int64
		if err := rows.Scan(&e.ID, &e.Name, &created, &expires, &consumed, &e.Metadata, &e.ResolvedID); err != nil {
			return nil, err
		}
		e.Created = time.Unix(0, created)
		e.Expires = time.Unix(0, expires)
		if consumed > 0 {
			e.Consumed = time.Unix(0, consumed)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RevokeEnrollment deletes an outstanding enrollment. No-op on missing id.
func (s *Storage) RevokeEnrollment(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM enrollments WHERE id = ?`, id)
	return err
}

// Enrollment errors are exported so handlers can map them to HTTP codes.
var (
	ErrEnrollmentNotFound  = errors.New("enrollment: token not found")
	ErrEnrollmentExpired   = errors.New("enrollment: token expired")
	ErrEnrollmentConsumed  = errors.New("enrollment: token already used")
	ErrEnrollmentNameTaken = errors.New("enrollment: an agent with that name already exists")
)

// ---------------------------------------------------------------------------
// Central policy (slice 0.2.7)
// ---------------------------------------------------------------------------

// PolicyRevision is one row of the policy_revisions audit log. The latest
// revision is what GET /api/policy + GET /agent/v1/policy return.
type PolicyRevision struct {
	ID            int64
	Body          []byte // raw JSON the operator submitted
	ETag          string // SHA-256 hex of Body
	Created       time.Time
	CreatedByNote string // free-form label, e.g. "dashboard" or admin's IP
}

// PutPolicy appends a new revision. Returns the resulting revision.
// The body is stored verbatim; parsing is deferred to the agent so a
// schema mismatch between central and a specific agent version doesn't
// brick the central side.
func (s *Storage) PutPolicy(ctx context.Context, body []byte, createdByNote string) (*PolicyRevision, error) {
	if len(body) == 0 {
		return nil, errors.New("policy: body is required")
	}
	etag := HashToken(string(body))
	now := time.Now().UnixNano()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO policy_revisions (body, etag, created_at, created_by)
		VALUES (?, ?, ?, ?)`,
		body, etag, now, createdByNote)
	if err != nil {
		return nil, fmt.Errorf("insert policy revision: %w", err)
	}
	id, _ := res.LastInsertId()
	return &PolicyRevision{
		ID:            id,
		Body:          body,
		ETag:          etag,
		Created:       time.Unix(0, now),
		CreatedByNote: createdByNote,
	}, nil
}

// LatestPolicy returns the most recent revision, or (nil, nil) if none.
func (s *Storage) LatestPolicy(ctx context.Context) (*PolicyRevision, error) {
	var (
		r           PolicyRevision
		created     int64
		createdByNS sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, body, etag, created_at, COALESCE(created_by, '')
		FROM policy_revisions ORDER BY id DESC LIMIT 1`).
		Scan(&r.ID, &r.Body, &r.ETag, &created, &createdByNS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.Created = time.Unix(0, created)
	if createdByNS.Valid {
		r.CreatedByNote = createdByNS.String
	}
	return &r, nil
}

// ListPolicyRevisions returns the audit history newest-first.
func (s *Storage) ListPolicyRevisions(ctx context.Context, limit int) ([]PolicyRevision, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, body, etag, created_at, COALESCE(created_by, '')
		FROM policy_revisions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PolicyRevision
	for rows.Next() {
		var r PolicyRevision
		var created int64
		if err := rows.Scan(&r.ID, &r.Body, &r.ETag, &created, &r.CreatedByNote); err != nil {
			return nil, err
		}
		r.Created = time.Unix(0, created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Event ingest
// ---------------------------------------------------------------------------

// IngestEvent is the wire shape of one event submitted by an agent.
// Mirrors internal/audit.Event but uses int64 timestamps for JSON
// transport-friendliness.
type IngestEvent struct {
	AgentTS   int64           `json:"agent_ts"`
	SessionID string          `json:"session_id"`
	Upstream  string          `json:"upstream"`
	Direction string          `json:"direction"`
	MsgType   string          `json:"msg_type"`
	MsgID     string          `json:"msg_id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	Bytes     int             `json:"bytes"`
}

// IngestBatch inserts a batch of events under the given agent id.
// Returns the number of rows actually inserted (some may be rejected as
// malformed; we don't fail the whole batch for one bad row).
func (s *Storage) IngestBatch(ctx context.Context, agentID int64, batch []IngestEvent) (int, error) {
	if len(batch) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO events
		(agent_id, agent_ts, server_ts, session_id, upstream, direction,
		 msg_type, msg_id, method, payload, bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	now := time.Now().UnixNano()
	inserted := 0
	for _, e := range batch {
		if e.AgentTS == 0 || e.SessionID == "" || e.Direction == "" || e.MsgType == "" {
			continue // skip malformed
		}
		_, err := stmt.ExecContext(ctx,
			agentID, e.AgentTS, now, e.SessionID, e.Upstream, e.Direction,
			e.MsgType, nullStr(e.MsgID), nullStr(e.Method), []byte(e.Payload), e.Bytes)
		if err != nil {
			continue
		}
		inserted++
	}
	if err := tx.Commit(); err != nil {
		return inserted, err
	}
	return inserted, nil
}

// ---------------------------------------------------------------------------
// Event queries (for the dashboard)
// ---------------------------------------------------------------------------

// EventFilter narrows ListEvents. Zero-valued fields disable that filter.
// All non-empty filters AND together.
type EventFilter struct {
	AgentID   int64
	SessionID string
	// Query is a substring matched against the raw JSON payload via
	// INSTR. Added in slice 0.2.4 for payload search.
	Query string
	Limit int
}

// ListEvents returns the most recent events that match the filter,
// ordered by id desc. Joined to agents for the agent name.
func (s *Storage) ListEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	query := `
		SELECT e.id, e.agent_id, a.name, e.agent_ts, e.server_ts,
		       e.session_id, e.upstream, e.direction, e.msg_type,
		       COALESCE(e.msg_id, ''), COALESCE(e.method, ''),
		       e.payload, e.bytes
		FROM events e
		JOIN agents a ON a.id = e.agent_id`
	var conds []string
	var args []any
	if f.AgentID > 0 {
		conds = append(conds, "e.agent_id = ?")
		args = append(args, f.AgentID)
	}
	if f.SessionID != "" {
		conds = append(conds, "e.session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.Query != "" {
		conds = append(conds, "INSTR(e.payload, ?) > 0")
		args = append(args, f.Query)
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY e.id DESC LIMIT ?"
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var agentTS, serverTS int64
		if err := rows.Scan(&e.ID, &e.AgentID, &e.AgentName, &agentTS, &serverTS,
			&e.SessionID, &e.Upstream, &e.Direction, &e.MsgType,
			&e.MsgID, &e.Method, &e.Payload, &e.Bytes); err != nil {
			return nil, err
		}
		e.AgentTS = time.Unix(0, agentTS)
		e.ServerTS = time.Unix(0, serverTS)
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetEvent returns a single event by id, with payload. Returns
// (nil, nil) if no row matches — callers should treat that as 404.
func (s *Storage) GetEvent(ctx context.Context, id int64) (*Event, error) {
	var (
		e                Event
		agentTS, serverTS int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT e.id, e.agent_id, a.name, e.agent_ts, e.server_ts,
		       e.session_id, e.upstream, e.direction, e.msg_type,
		       COALESCE(e.msg_id, ''), COALESCE(e.method, ''),
		       e.payload, e.bytes
		FROM events e
		JOIN agents a ON a.id = e.agent_id
		WHERE e.id = ?`, id).
		Scan(&e.ID, &e.AgentID, &e.AgentName, &agentTS, &serverTS,
			&e.SessionID, &e.Upstream, &e.Direction, &e.MsgType,
			&e.MsgID, &e.Method, &e.Payload, &e.Bytes)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	e.AgentTS = time.Unix(0, agentTS)
	e.ServerTS = time.Unix(0, serverTS)
	return &e, nil
}

// SessionRow is one aggregated row in the dashboard's Sessions section.
// A session is unique per agent: the same session_id reported by two
// different agents is two rows.
type SessionRow struct {
	SessionID    string
	AgentID      int64
	AgentName    string
	FirstTS      time.Time
	LastTS       time.Time
	EventCount   int64
	BlockedCount int64
	// Upstreams is the comma-separated list of distinct upstream names
	// touched during the session (usually a single value, sometimes more
	// when a client multiplexes through one sentinel process).
	Upstreams string
}

// ListSessions aggregates events into sessions. Optional agent filter.
// Ordered by most-recent activity desc.
func (s *Storage) ListSessions(ctx context.Context, agentID int64, limit int) ([]SessionRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `
		SELECT e.session_id,
		       e.agent_id,
		       a.name,
		       MIN(e.server_ts),
		       MAX(e.server_ts),
		       COUNT(*) AS event_count,
		       SUM(CASE WHEN e.direction='s2c' AND e.msg_type='error'
		                 AND INSTR(e.payload, 'blocked by sentinel policy') > 0
		                THEN 1 ELSE 0 END) AS blocked_count,
		       COALESCE(GROUP_CONCAT(DISTINCT e.upstream), '')
		FROM events e
		JOIN agents a ON a.id = e.agent_id`
	args := []any{}
	if agentID > 0 {
		query += " WHERE e.agent_id = ?"
		args = append(args, agentID)
	}
	query += `
		GROUP BY e.session_id, e.agent_id, a.name
		ORDER BY MAX(e.server_ts) DESC
		LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		var first, last int64
		if err := rows.Scan(&r.SessionID, &r.AgentID, &r.AgentName,
			&first, &last, &r.EventCount, &r.BlockedCount, &r.Upstreams); err != nil {
			return nil, err
		}
		r.FirstTS = time.Unix(0, first)
		r.LastTS = time.Unix(0, last)
		out = append(out, r)
	}
	return out, rows.Err()
}

// EventStats is a small aggregate the dashboard renders.
type EventStats struct {
	Total        int64
	Last24h      int64
	BlockedCount int64
	AgentCount   int64
}

// Stats returns counts for the dashboard's top bar.
func (s *Storage) Stats(ctx context.Context) (EventStats, error) {
	var st EventStats
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&st.Total)
	_ = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events
		 WHERE server_ts > (strftime('%s','now') - 86400) * 1000000000`).Scan(&st.Last24h)
	_ = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events
		 WHERE direction='s2c' AND msg_type='error'
		   AND INSTR(payload, 'blocked by sentinel policy') > 0`).Scan(&st.BlockedCount)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents`).Scan(&st.AgentCount)
	return st, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const schema = `
CREATE TABLE IF NOT EXISTS agents (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    token_hash  TEXT NOT NULL UNIQUE,
    created_at  INTEGER NOT NULL,
    last_seen   INTEGER,
    metadata    TEXT
);

CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id    INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    agent_ts    INTEGER NOT NULL,
    server_ts   INTEGER NOT NULL,
    session_id  TEXT NOT NULL,
    upstream    TEXT NOT NULL,
    direction   TEXT NOT NULL,
    msg_type    TEXT NOT NULL,
    msg_id      TEXT,
    method      TEXT,
    payload     BLOB NOT NULL,
    bytes       INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_agent      ON events(agent_id);
CREATE INDEX IF NOT EXISTS idx_events_server_ts  ON events(server_ts);
CREATE INDEX IF NOT EXISTS idx_events_method     ON events(method);
CREATE INDEX IF NOT EXISTS idx_events_session    ON events(session_id);

CREATE TABLE IF NOT EXISTS enrollments (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL,
    ott_hash            TEXT NOT NULL UNIQUE,
    created_at          INTEGER NOT NULL,
    expires_at          INTEGER NOT NULL,
    consumed_at         INTEGER,
    resolved_agent_id   INTEGER,
    metadata            TEXT
);
CREATE INDEX IF NOT EXISTS idx_enrollments_hash  ON enrollments(ott_hash);

CREATE TABLE IF NOT EXISTS policy_revisions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    body        BLOB NOT NULL,
    etag        TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    created_by  TEXT
);
CREATE INDEX IF NOT EXISTS idx_policy_created_at ON policy_revisions(created_at);
`
