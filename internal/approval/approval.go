// Package approval manages the pending-approval queue.
//
// Slice 4 introduces a third decision path between "allow" and "block":
// when a tool call lands in the gray zone (risk score >= approve_threshold
// but < block_threshold), the proxy SUSPENDS the call, writes a pending row
// here, and polls until a human approves or denies via the dashboard —
// or the configured timeout expires (default deny).
//
// State lives in the same SQLite DB as the audit log so:
//   - one source of truth, no cross-process coordination problem
//   - the queue persists across proxy restarts (the suspended call won't,
//     because the client connection is gone, but the audit row stays)
//   - the dashboard reads/writes the queue without needing IPC to the proxy
package approval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Status of a pending approval row.
type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusDenied   Status = "denied"
	StatusTimeout  Status = "timeout"
)

// Approval is one row in the approvals table.
type Approval struct {
	ID            int64
	CreatedAt     time.Time
	SessionID     string
	Upstream      string
	MsgID         string // original JSON-RPC id
	Method        string
	ToolName      string
	RiskScore     int
	FindingsJSON  []byte // raw JSON array of policy findings
	Payload       []byte // raw original request envelope
	Status        Status
	ResolvedAt    time.Time
	ResolvedBy    string
}

// Store wraps the approvals table.
type Store struct {
	db *sql.DB
}

// Open opens (or initializes) the approvals table at the given DB path.
// The path is the same SQLite file used by the audit log.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

// OpenWithDB reuses an existing *sql.DB (used by tests that already opened
// the audit log).
func OpenWithDB(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying DB handle (only if Store opened it).
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Insert creates a new pending row and returns its id.
func (s *Store) Insert(ctx context.Context, a Approval) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals
		(created_at, session_id, upstream, msg_id, method, tool_name,
		 risk_score, findings_json, payload, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.CreatedAt.UnixNano(),
		a.SessionID, a.Upstream, a.MsgID, a.Method, a.ToolName,
		a.RiskScore, a.FindingsJSON, a.Payload, string(StatusPending),
	)
	if err != nil {
		return 0, fmt.Errorf("insert approval: %w", err)
	}
	return res.LastInsertId()
}

// Get returns the row at id.
func (s *Store) Get(ctx context.Context, id int64) (*Approval, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, created_at, session_id, upstream, msg_id, method, tool_name,
		       risk_score, findings_json, payload, status,
		       COALESCE(resolved_at, 0), COALESCE(resolved_by, '')
		FROM approvals WHERE id = ?`, id)
	return scanApproval(row)
}

// ListPending returns up to limit pending rows ordered oldest-first.
func (s *Store) ListPending(ctx context.Context, limit int) ([]Approval, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at, session_id, upstream, msg_id, method, tool_name,
		       risk_score, findings_json, payload, status,
		       COALESCE(resolved_at, 0), COALESCE(resolved_by, '')
		FROM approvals
		WHERE status = ?
		ORDER BY id ASC
		LIMIT ?`, string(StatusPending), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApprovalRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// Resolve transitions a pending row to the given final status.
// Returns an error if the row is not pending or does not exist.
func (s *Store) Resolve(ctx context.Context, id int64, status Status, by string) error {
	if status != StatusApproved && status != StatusDenied && status != StatusTimeout {
		return fmt.Errorf("invalid resolution status %q", status)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE approvals
		SET status = ?, resolved_at = ?, resolved_by = ?
		WHERE id = ? AND status = ?`,
		string(status), time.Now().UnixNano(), by, id, string(StatusPending),
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either no such id or already resolved.
		var current string
		err := s.db.QueryRowContext(ctx, `SELECT status FROM approvals WHERE id = ?`, id).Scan(&current)
		if err != nil {
			return fmt.Errorf("approval %d not found", id)
		}
		return fmt.Errorf("approval %d already resolved (status=%s)", id, current)
	}
	return nil
}

// WaitForDecision blocks until the row's status is no longer pending or the
// context is done. Returns the final status. If the context is done with the
// row still pending, returns StatusTimeout without writing it (caller may
// choose to call Resolve(StatusTimeout) to record the outcome).
func (s *Store) WaitForDecision(ctx context.Context, id int64, pollInterval time.Duration) (Status, error) {
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		// Use a background context for the query so a cancelled parent
		// ctx still gets one last status check and a clean Timeout return
		// instead of a "context deadline exceeded" error.
		queryCtx, qcancel := context.WithTimeout(context.Background(), 2*time.Second)
		var status string
		err := s.db.QueryRowContext(queryCtx, `SELECT status FROM approvals WHERE id = ?`, id).Scan(&status)
		qcancel()
		if err != nil {
			return "", fmt.Errorf("poll status for %d: %w", id, err)
		}
		if status != string(StatusPending) {
			return Status(status), nil
		}
		select {
		case <-ctx.Done():
			return StatusTimeout, nil
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanApproval(r rowScanner) (*Approval, error) {
	var (
		a          Approval
		createdAt  int64
		resolvedAt int64
		status     string
	)
	err := r.Scan(&a.ID, &createdAt, &a.SessionID, &a.Upstream, &a.MsgID,
		&a.Method, &a.ToolName, &a.RiskScore, &a.FindingsJSON, &a.Payload,
		&status, &resolvedAt, &a.ResolvedBy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	a.CreatedAt = time.Unix(0, createdAt)
	if resolvedAt > 0 {
		a.ResolvedAt = time.Unix(0, resolvedAt)
	}
	a.Status = Status(status)
	return &a, nil
}

func scanApprovalRows(rows *sql.Rows) (*Approval, error) {
	return scanApproval(rows)
}

// MarshalFindings serializes any value as a JSON BLOB suitable for the
// FindingsJSON field. Callers use this to keep the findings payload structured.
func MarshalFindings(v any) ([]byte, error) {
	return json.Marshal(v)
}

// ---------------------------------------------------------------------------
// Auto-decisions: persistent allow/deny rulings on a given rule_id.
// ---------------------------------------------------------------------------

// AutoDecision is one persistent ruling on a rule_id ("category/rule").
type AutoDecision struct {
	RuleID    string
	Decision  Status // StatusApproved (allow) or StatusDenied (deny)
	CreatedAt time.Time
	CreatedBy string
	Note      string
}

// SetAutoDecision creates or replaces an auto-decision for ruleID.
// Decision must be StatusApproved or StatusDenied.
func (s *Store) SetAutoDecision(ctx context.Context, ruleID string, decision Status, by, note string) error {
	if ruleID == "" {
		return fmt.Errorf("rule_id is required")
	}
	if decision != StatusApproved && decision != StatusDenied {
		return fmt.Errorf("auto-decision must be approved or denied, got %q", decision)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auto_decisions (rule_id, decision, created_at, created_by, note)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(rule_id) DO UPDATE SET
		    decision   = excluded.decision,
		    created_at = excluded.created_at,
		    created_by = excluded.created_by,
		    note       = excluded.note`,
		ruleID, string(decision), time.Now().UnixNano(), by, nullStr(note))
	return err
}

// GetAutoDecision returns the current decision for ruleID, or ("", false) if
// none exists.
func (s *Store) GetAutoDecision(ctx context.Context, ruleID string) (Status, bool, error) {
	var d string
	err := s.db.QueryRowContext(ctx, `SELECT decision FROM auto_decisions WHERE rule_id = ?`, ruleID).Scan(&d)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return Status(d), true, nil
}

// ListAutoDecisions returns every persisted auto-decision, ordered by
// rule_id ascending.
func (s *Store) ListAutoDecisions(ctx context.Context) ([]AutoDecision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rule_id, decision, created_at, created_by, COALESCE(note, '')
		FROM auto_decisions ORDER BY rule_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AutoDecision
	for rows.Next() {
		var ad AutoDecision
		var ts int64
		var dec string
		if err := rows.Scan(&ad.RuleID, &dec, &ts, &ad.CreatedBy, &ad.Note); err != nil {
			return nil, err
		}
		ad.Decision = Status(dec)
		ad.CreatedAt = time.Unix(0, ts)
		out = append(out, ad)
	}
	return out, rows.Err()
}

// DeleteAutoDecision removes the rule_id's auto-decision (a no-op if none).
func (s *Store) DeleteAutoDecision(ctx context.Context, ruleID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auto_decisions WHERE rule_id = ?`, ruleID)
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const schema = `
CREATE TABLE IF NOT EXISTS approvals (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at     INTEGER NOT NULL,
    session_id     TEXT NOT NULL,
    upstream       TEXT NOT NULL,
    msg_id         TEXT NOT NULL,
    method         TEXT NOT NULL,
    tool_name      TEXT,
    risk_score     INTEGER NOT NULL,
    findings_json  BLOB NOT NULL,
    payload        BLOB NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending',
    resolved_at    INTEGER,
    resolved_by    TEXT
);

CREATE INDEX IF NOT EXISTS idx_approvals_status     ON approvals(status);
CREATE INDEX IF NOT EXISTS idx_approvals_created_at ON approvals(created_at);
CREATE INDEX IF NOT EXISTS idx_approvals_session    ON approvals(session_id);

CREATE TABLE IF NOT EXISTS auto_decisions (
    rule_id     TEXT PRIMARY KEY,        -- "category/rule"
    decision    TEXT NOT NULL,           -- 'allow' or 'deny'
    created_at  INTEGER NOT NULL,
    created_by  TEXT NOT NULL,
    note        TEXT
);
`
