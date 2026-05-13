package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ironmark056/sentinel/internal/approval"
)

// Stats is what /api/stats returns.
type Stats struct {
	Total          int64           `json:"total"`
	Last24h        int64           `json:"last_24h"`
	BlockedCount   int64           `json:"blocked_count"`
	Sessions       int64           `json:"sessions"`
	TopMethods     []MethodCount   `json:"top_methods"`
	TopUpstreams   []UpstreamCount `json:"top_upstreams"`
	RecentBlocked  []EventSummary  `json:"recent_blocked"`
	AuditDBPath    string          `json:"audit_db_path"`
}

// MethodCount appears in Stats.TopMethods.
type MethodCount struct {
	Method string `json:"method"`
	Count  int64  `json:"count"`
}

// UpstreamCount appears in Stats.TopUpstreams.
type UpstreamCount struct {
	Upstream string `json:"upstream"`
	Count    int64  `json:"count"`
}

// EventSummary is a lightweight row used in listings.
type EventSummary struct {
	ID        int64  `json:"id"`
	TS        int64  `json:"ts"` // unix nanos
	SessionID string `json:"session_id"`
	Upstream  string `json:"upstream"`
	Direction string `json:"direction"`
	MsgType   string `json:"msg_type"`
	MsgID     string `json:"msg_id"`
	Method    string `json:"method"`
	Bytes     int    `json:"bytes"`
}

// EventDetail is one full audit row, including raw payload.
type EventDetail struct {
	EventSummary
	Payload string `json:"payload"` // raw JSON-RPC envelope (string)
}

// handleStats — GET /api/stats.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := Stats{AuditDBPath: s.opts.AuditPath}

	q := s.db.QueryRow(`SELECT COUNT(*) FROM messages`)
	if err := q.Scan(&stats.Total); err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Last 24 hours (ts is in unix nanos).
	q = s.db.QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE ts > (strftime('%s','now') - 86400) * 1000000000`)
	if err := q.Scan(&stats.Last24h); err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Blocked = synthesized error responses our proxy emits (msg_type='error'
	// going s2c whose payload contains 'blocked by sentinel policy').
	q = s.db.QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE direction='s2c' AND msg_type='error'
		  AND INSTR(payload, 'blocked by sentinel policy') > 0`)
	if err := q.Scan(&stats.BlockedCount); err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Distinct sessions in the last 24 hours.
	q = s.db.QueryRow(`
		SELECT COUNT(DISTINCT session_id) FROM messages
		WHERE ts > (strftime('%s','now') - 86400) * 1000000000`)
	if err := q.Scan(&stats.Sessions); err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Top methods.
	rows, err := s.db.Query(`
		SELECT COALESCE(method, '(no method)') AS m, COUNT(*) AS c
		FROM messages
		WHERE method IS NOT NULL AND method != ''
		GROUP BY m ORDER BY c DESC LIMIT 10`)
	if err == nil {
		for rows.Next() {
			var mc MethodCount
			if err := rows.Scan(&mc.Method, &mc.Count); err == nil {
				stats.TopMethods = append(stats.TopMethods, mc)
			}
		}
		_ = rows.Close()
	}

	// Top upstreams.
	rows, err = s.db.Query(`
		SELECT upstream, COUNT(*) AS c
		FROM messages
		GROUP BY upstream ORDER BY c DESC LIMIT 10`)
	if err == nil {
		for rows.Next() {
			var uc UpstreamCount
			if err := rows.Scan(&uc.Upstream, &uc.Count); err == nil {
				stats.TopUpstreams = append(stats.TopUpstreams, uc)
			}
		}
		_ = rows.Close()
	}

	// Five most recent blocked calls — useful glanceable summary.
	rows, err = s.db.Query(`
		SELECT id, ts, session_id, upstream, direction, msg_type,
		       COALESCE(msg_id,''), COALESCE(method,''), bytes
		FROM messages
		WHERE direction='s2c' AND msg_type='error'
		  AND INSTR(payload, 'blocked by sentinel policy') > 0
		ORDER BY id DESC LIMIT 5`)
	if err == nil {
		for rows.Next() {
			var e EventSummary
			if err := rows.Scan(&e.ID, &e.TS, &e.SessionID, &e.Upstream,
				&e.Direction, &e.MsgType, &e.MsgID, &e.Method, &e.Bytes); err == nil {
				stats.RecentBlocked = append(stats.RecentBlocked, e)
			}
		}
		_ = rows.Close()
	}

	writeJSON(w, stats)
}

// handleEvents — GET /api/events?limit=N&before_id=ID
// Returns events ordered by id desc (most recent first), paginated.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	beforeID := int64(0)
	if v := r.URL.Query().Get("before_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			beforeID = n
		}
	}

	args := []any{}
	where := ""
	if beforeID > 0 {
		where = "WHERE id < ?"
		args = append(args, beforeID)
	}
	args = append(args, limit)

	rows, err := s.db.Query(`
		SELECT id, ts, session_id, upstream, direction, msg_type,
		       COALESCE(msg_id,''), COALESCE(method,''), bytes
		FROM messages `+where+`
		ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	var out []EventSummary
	for rows.Next() {
		var e EventSummary
		if err := rows.Scan(&e.ID, &e.TS, &e.SessionID, &e.Upstream,
			&e.Direction, &e.MsgType, &e.MsgID, &e.Method, &e.Bytes); err != nil {
			s.writeError(w, http.StatusInternalServerError, err)
			return
		}
		out = append(out, e)
	}
	if out == nil {
		out = []EventSummary{}
	}
	writeJSON(w, out)
}

// handleEventDetail — GET /api/event/{id}
func (s *Server) handleEventDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/api/event/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	var ev EventDetail
	var payload []byte
	row := s.db.QueryRow(`
		SELECT id, ts, session_id, upstream, direction, msg_type,
		       COALESCE(msg_id,''), COALESCE(method,''), bytes, payload
		FROM messages WHERE id = ?`, id)
	if err := row.Scan(&ev.ID, &ev.TS, &ev.SessionID, &ev.Upstream,
		&ev.Direction, &ev.MsgType, &ev.MsgID, &ev.Method, &ev.Bytes, &payload); err != nil {
		if errors.Is(err, errors.New("sql: no rows in result set")) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ev.Payload = string(payload)
	writeJSON(w, ev)
}

// PendingApproval is the wire shape returned by /api/approvals.
type PendingApproval struct {
	ID         int64           `json:"id"`
	CreatedAt  int64           `json:"created_at"` // unix nanos
	SessionID  string          `json:"session_id"`
	Upstream   string          `json:"upstream"`
	MsgID      string          `json:"msg_id"`
	Method     string          `json:"method"`
	ToolName   string          `json:"tool_name"`
	RiskScore  int             `json:"risk_score"`
	Findings   json.RawMessage `json:"findings"`
	Payload    string          `json:"payload"`
}

// handleApprovals — GET /api/approvals
// Returns pending approval rows, oldest first (the queue order users should
// see).
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	list, err := s.approvals.ListPending(r.Context(), 100)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]PendingApproval, 0, len(list))
	for _, a := range list {
		out = append(out, PendingApproval{
			ID:        a.ID,
			CreatedAt: a.CreatedAt.UnixNano(),
			SessionID: a.SessionID,
			Upstream:  a.Upstream,
			MsgID:     a.MsgID,
			Method:    a.Method,
			ToolName:  a.ToolName,
			RiskScore: a.RiskScore,
			Findings:  json.RawMessage(a.FindingsJSON),
			Payload:   string(a.Payload),
		})
	}
	writeJSON(w, out)
}

// handleApprovalAction — POST /api/approvals/{id}/{approve|deny}
//
// Localhost-only and same-origin by design; CSRF protection is intentionally
// out of scope at this layer (the bind address is 127.0.0.1).
func (s *Server) handleApprovalAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/approvals/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "use POST /api/approvals/{id}/approve|deny", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	var status approval.Status
	switch parts[1] {
	case "approve":
		status = approval.StatusApproved
	case "deny":
		status = approval.StatusDenied
	default:
		http.Error(w, "action must be 'approve' or 'deny'", http.StatusBadRequest)
		return
	}

	by := r.URL.Query().Get("by")
	if by == "" {
		by = "dashboard"
	}

	// "remember=true" promotes the decision into a persistent auto-decision
	// for every rule_id present in this approval's findings.
	remember := r.URL.Query().Get("remember") == "true"

	if remember {
		// Fetch the row first so we know which rules to record.
		row, ferr := s.approvals.Get(r.Context(), id)
		if ferr != nil {
			s.writeError(w, http.StatusInternalServerError, ferr)
			return
		}
		if row == nil {
			http.Error(w, "approval not found", http.StatusNotFound)
			return
		}
		var findings []struct {
			Category string `json:"Category"`
			Rule     string `json:"Rule"`
		}
		_ = json.Unmarshal(row.FindingsJSON, &findings)
		seen := map[string]bool{}
		for _, f := range findings {
			ruleID := f.Category + "/" + f.Rule
			if ruleID == "/" || seen[ruleID] {
				continue
			}
			seen[ruleID] = true
			if err := s.approvals.SetAutoDecision(r.Context(), ruleID, status, by, ""); err != nil {
				s.logger.Printf("failed to set auto-decision for %s: %v", ruleID, err)
			} else {
				s.logger.Printf("auto-decision SET rule=%s decision=%s by=%s", ruleID, status, by)
			}
		}
	}

	if err := s.approvals.Resolve(r.Context(), id, status, by); err != nil {
		if strings.Contains(err.Error(), "already resolved") || strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{
		"id":       parts[0],
		"status":   string(status),
		"by":       by,
		"remember": remember,
	})
}

// AutoDecisionDTO is the wire shape for /api/auto-decisions.
type AutoDecisionDTO struct {
	RuleID    string `json:"rule_id"`
	Decision  string `json:"decision"`
	CreatedAt int64  `json:"created_at"`
	CreatedBy string `json:"created_by"`
	Note      string `json:"note,omitempty"`
}

// handleAutoDecisionsList — GET /api/auto-decisions
//
// Returns every persisted auto-decision so the dashboard can render a
// management view.
func (s *Server) handleAutoDecisionsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	list, err := s.approvals.ListAutoDecisions(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]AutoDecisionDTO, 0, len(list))
	for _, a := range list {
		out = append(out, AutoDecisionDTO{
			RuleID:    a.RuleID,
			Decision:  string(a.Decision),
			CreatedAt: a.CreatedAt.UnixNano(),
			CreatedBy: a.CreatedBy,
			Note:      a.Note,
		})
	}
	writeJSON(w, out)
}

// handleAutoDecisionDelete — DELETE /api/auto-decisions/{rule_id}
//
// URL-decoded rule_id is removed. Returns 204 even if it didn't exist
// (idempotent delete).
func (s *Server) handleAutoDecisionDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ruleID := strings.TrimPrefix(r.URL.Path, "/api/auto-decisions/")
	if ruleID == "" {
		http.Error(w, "rule_id required", http.StatusBadRequest)
		return
	}
	if err := s.approvals.DeleteAutoDecision(r.Context(), ruleID); err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.logger.Printf("auto-decision REMOVED rule=%s", ruleID)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeError(w http.ResponseWriter, code int, err error) {
	s.logger.Printf("api error: %v", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
