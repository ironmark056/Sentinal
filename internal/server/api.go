package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Auth middleware
// ---------------------------------------------------------------------------

type agentCtxKey struct{}

// requireAgent extracts the bearer token from Authorization and resolves
// it to an Agent. 401 on missing/invalid.
func (s *Server) requireAgent(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		agent, err := s.store.LookupAgentByToken(r.Context(), token)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, err)
			return
		}
		if agent == nil {
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), agentCtxKey{}, agent)
		h(w, r.WithContext(ctx))
	}
}

// requireAdmin gates write operations on the dashboard. When AdminToken
// is unset on the Server, this is a no-op (deployments behind a trusted
// network may run without one).
func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.AdminToken == "" {
			h(w, r)
			return
		}
		if bearerToken(r) != s.opts.AdminToken {
			http.Error(w, "admin token required", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// ---------------------------------------------------------------------------
// Open endpoints
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"version": Version})
}

// Version is set at build time via -ldflags. Defaults to "dev".
var Version = "dev"

// ---------------------------------------------------------------------------
// Agent-side endpoints (bearer-token gated)
// ---------------------------------------------------------------------------

// IngestRequest is the body of POST /agent/v1/events.
type IngestRequest struct {
	Events []IngestEvent `json:"events"`
}

// IngestResponse is the response body.
type IngestResponse struct {
	Accepted int `json:"accepted"`
}

func (s *Server) handleAgentEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agent := r.Context().Value(agentCtxKey{}).(*Agent)

	var req IngestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	n, err := s.store.IngestBatch(r.Context(), agent.ID, req.Events)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, IngestResponse{Accepted: n})
}

// handleAgentHealth lets an agent check its connectivity / token before
// pushing events. Returns the agent's own identity.
func (s *Server) handleAgentHealth(w http.ResponseWriter, r *http.Request) {
	agent := r.Context().Value(agentCtxKey{}).(*Agent)
	writeJSON(w, map[string]any{
		"ok":         true,
		"agent_id":   agent.ID,
		"agent_name": agent.Name,
	})
}

// ---------------------------------------------------------------------------
// Dashboard endpoints
// ---------------------------------------------------------------------------

type StatsDTO struct {
	Total        int64 `json:"total"`
	Last24h      int64 `json:"last_24h"`
	BlockedCount int64 `json:"blocked_count"`
	AgentCount   int64 `json:"agent_count"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st, err := s.store.Stats(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, StatsDTO{
		Total: st.Total, Last24h: st.Last24h,
		BlockedCount: st.BlockedCount, AgentCount: st.AgentCount,
	})
}

type AgentDTO struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Created  int64  `json:"created"`
	LastSeen int64  `json:"last_seen"`
	Metadata string `json:"metadata,omitempty"`
}

// handleAgents — GET (list) or POST (create, admin-only).
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listAgents(w, r)
	case http.MethodPost:
		s.requireAdmin(s.createAgent).ServeHTTP(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListAgents(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]AgentDTO, 0, len(list))
	for _, a := range list {
		out = append(out, AgentDTO{
			ID:       a.ID,
			Name:     a.Name,
			Created:  a.Created.UnixNano(),
			LastSeen: zeroTime(a.LastSeen),
			Metadata: string(a.Metadata),
		})
	}
	writeJSON(w, out)
}

// createAgent — POST /api/agents body: {name, metadata?}
// Response: {agent: {...}, token: "mcpg_..."}  // token shown ONCE.
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string            `json:"name"`
		Metadata map[string]string `json:"metadata,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	token, agent, err := s.store.CreateAgent(r.Context(), req.Name, req.Metadata)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			http.Error(w, "agent name already exists", http.StatusConflict)
			return
		}
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{
		"agent": AgentDTO{
			ID: agent.ID, Name: agent.Name,
			Created:  agent.Created.UnixNano(),
			Metadata: string(agent.Metadata),
		},
		"token": token,
		"note":  "Save this token; it will not be shown again.",
	})
}

// handleAgentMutate routes /api/agents/{id} DELETE.
func (s *Server) handleAgentMutate(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/agents/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.store.DeleteAgent(r.Context(), id); err != nil {
			s.writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// EventDTO is the dashboard wire shape.
type EventDTO struct {
	ID        int64  `json:"id"`
	AgentID   int64  `json:"agent_id"`
	AgentName string `json:"agent_name"`
	AgentTS   int64  `json:"agent_ts"`
	ServerTS  int64  `json:"server_ts"`
	SessionID string `json:"session_id"`
	Upstream  string `json:"upstream"`
	Direction string `json:"direction"`
	MsgType   string `json:"msg_type"`
	MsgID     string `json:"msg_id"`
	Method    string `json:"method"`
	Bytes     int    `json:"bytes"`
	Payload   string `json:"payload,omitempty"`
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	agentID := int64(0)
	if v := q.Get("agent_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			agentID = n
		}
	}
	includePayload := q.Get("with_payload") == "true"

	list, err := s.store.ListEvents(r.Context(), EventFilter{
		AgentID:   agentID,
		SessionID: q.Get("session_id"),
		Query:     q.Get("q"),
		Limit:     limit,
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]EventDTO, 0, len(list))
	for _, e := range list {
		d := EventDTO{
			ID:        e.ID,
			AgentID:   e.AgentID,
			AgentName: e.AgentName,
			AgentTS:   e.AgentTS.UnixNano(),
			ServerTS:  e.ServerTS.UnixNano(),
			SessionID: e.SessionID,
			Upstream:  e.Upstream,
			Direction: e.Direction,
			MsgType:   e.MsgType,
			MsgID:     e.MsgID,
			Method:    e.Method,
			Bytes:     e.Bytes,
		}
		if includePayload {
			d.Payload = string(e.Payload)
		}
		out = append(out, d)
	}
	writeJSON(w, out)
}

// SessionDTO is the wire shape returned by /api/sessions.
type SessionDTO struct {
	SessionID    string `json:"session_id"`
	AgentID      int64  `json:"agent_id"`
	AgentName    string `json:"agent_name"`
	FirstTS      int64  `json:"first_ts"`
	LastTS       int64  `json:"last_ts"`
	EventCount   int64  `json:"event_count"`
	BlockedCount int64  `json:"blocked_count"`
	// Upstreams is a comma-separated list as produced by GROUP_CONCAT.
	// The SPA splits on "," for display.
	Upstreams string `json:"upstreams"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	agentID := int64(0)
	if v := q.Get("agent_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			agentID = n
		}
	}
	rows, err := s.store.ListSessions(r.Context(), agentID, limit)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]SessionDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, SessionDTO{
			SessionID:    row.SessionID,
			AgentID:      row.AgentID,
			AgentName:    row.AgentName,
			FirstTS:      row.FirstTS.UnixNano(),
			LastTS:       row.LastTS.UnixNano(),
			EventCount:   row.EventCount,
			BlockedCount: row.BlockedCount,
			Upstreams:    row.Upstreams,
		})
	}
	writeJSON(w, out)
}

// ---------------------------------------------------------------------------
// Central policy (slice 0.2.7)
// ---------------------------------------------------------------------------

// PolicyDTO is the wire shape for /api/policy GET.
type PolicyDTO struct {
	ID        int64           `json:"id"`
	ETag      string          `json:"etag"`
	Created   int64           `json:"created"`
	CreatedBy string          `json:"created_by,omitempty"`
	Body      json.RawMessage `json:"body"`
}

// handlePolicy — GET (read latest, public) or PUT (write, admin).
func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getPolicy(w, r)
	case http.MethodPut:
		s.requireAdmin(s.putPolicy).ServeHTTP(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request) {
	rev, err := s.store.LatestPolicy(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rev == nil {
		// No policy set yet — return an empty body so agents know there's
		// nothing to merge. 200 (not 404) keeps the dashboard simple.
		writeJSON(w, PolicyDTO{Body: json.RawMessage(`{}`)})
		return
	}
	policyETagHeader(w, rev.ETag)
	writeJSON(w, PolicyDTO{
		ID:        rev.ID,
		ETag:      rev.ETag,
		Created:   rev.Created.UnixNano(),
		CreatedBy: rev.CreatedByNote,
		Body:      rev.Body,
	})
}

func (s *Server) putPolicy(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Sanity-check it's at least valid JSON; structural validation is
	// the agent's job so a richer-schema future agent can submit policies
	// older servers don't understand without us bouncing them here.
	var probe any
	if err := json.Unmarshal(body, &probe); err != nil {
		http.Error(w, "body must be valid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	note := r.Header.Get("X-Sentinel-Editor")
	if note == "" {
		note = "dashboard"
	}
	rev, err := s.store.PutPolicy(r.Context(), body, note)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	policyETagHeader(w, rev.ETag)
	writeJSON(w, PolicyDTO{
		ID:        rev.ID,
		ETag:      rev.ETag,
		Created:   rev.Created.UnixNano(),
		CreatedBy: rev.CreatedByNote,
		Body:      rev.Body,
	})
}

func (s *Server) handlePolicyRevisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	list, err := s.store.ListPolicyRevisions(r.Context(), limit)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]PolicyDTO, 0, len(list))
	for _, rev := range list {
		out = append(out, PolicyDTO{
			ID:        rev.ID,
			ETag:      rev.ETag,
			Created:   rev.Created.UnixNano(),
			CreatedBy: rev.CreatedByNote,
			Body:      rev.Body,
		})
	}
	writeJSON(w, out)
}

// handleAgentPolicy — GET /agent/v1/policy, bearer-agent-token auth.
// Honors If-None-Match for cheap polling.
func (s *Server) handleAgentPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rev, err := s.store.LatestPolicy(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rev == nil {
		// Empty policy — 200 with an empty object so the agent has a
		// distinct "central is reachable but says no policy" signal vs
		// "central is unreachable, use cache."
		writeJSON(w, PolicyDTO{Body: json.RawMessage(`{}`)})
		return
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == rev.ETag {
		policyETagHeader(w, rev.ETag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	policyETagHeader(w, rev.ETag)
	writeJSON(w, PolicyDTO{
		ID:        rev.ID,
		ETag:      rev.ETag,
		Created:   rev.Created.UnixNano(),
		CreatedBy: rev.CreatedByNote,
		Body:      rev.Body,
	})
}

func policyETagHeader(w http.ResponseWriter, etag string) {
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
}

// ---------------------------------------------------------------------------
// Enrollments — admin-side CRUD + public consume
// ---------------------------------------------------------------------------

// EnrollmentDTO is the wire shape used by the admin endpoints.
type EnrollmentDTO struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Created    int64  `json:"created"`
	Expires    int64  `json:"expires"`
	Consumed   int64  `json:"consumed,omitempty"`
	Metadata   string `json:"metadata,omitempty"`
	ResolvedID int64  `json:"resolved_agent_id,omitempty"`
}

// handleEnroll routes /api/enroll. GET = list, POST = create (admin).
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listEnrollments(w, r)
	case http.MethodPost:
		s.requireAdmin(s.createEnrollment).ServeHTTP(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listEnrollments(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListEnrollments(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]EnrollmentDTO, 0, len(list))
	for _, e := range list {
		dto := EnrollmentDTO{
			ID:         e.ID,
			Name:       e.Name,
			Created:    e.Created.UnixNano(),
			Expires:    e.Expires.UnixNano(),
			Metadata:   string(e.Metadata),
			ResolvedID: e.ResolvedID,
		}
		if !e.Consumed.IsZero() {
			dto.Consumed = e.Consumed.UnixNano()
		}
		out = append(out, dto)
	}
	writeJSON(w, out)
}

// createEnrollment — POST /api/enroll body: {name, ttl_seconds?, metadata?}
// Response includes both the OTT and the full URL the employee should run.
func (s *Server) createEnrollment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string            `json:"name"`
		TTLSeconds int               `json:"ttl_seconds,omitempty"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	ttl := 24 * time.Hour
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	ott, e, err := s.store.CreateEnrollment(r.Context(), req.Name, ttl, req.Metadata)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	url := publicBaseURL(r) + "/e/" + ott
	writeJSON(w, map[string]any{
		"enrollment": EnrollmentDTO{
			ID:       e.ID,
			Name:     e.Name,
			Created:  e.Created.UnixNano(),
			Expires:  e.Expires.UnixNano(),
			Metadata: string(e.Metadata),
		},
		"ott":     ott,
		"url":     url,
		"command": "sentinel enroll " + url,
		"note":    "Send the URL or the full command to the employee. Single-use; expires at the time shown.",
	})
}

// handleEnrollMutate routes /api/enroll/{id} DELETE for revocation.
// Admin-gated by the route registration.
func (s *Server) handleEnrollMutate(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/enroll/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.store.RevokeEnrollment(r.Context(), id); err != nil {
			s.writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleEnrollConsume — POST /e/{ott}, public, no auth. The OTT IS the
// credential. Returns the bearer token, agent identity, and a YAML
// fragment the client CLI can paste straight into the user's config.
func (s *Server) handleEnrollConsume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		// We tolerate GET for human-debugging-via-curl convenience; the
		// real client uses POST so a browser preview doesn't accidentally
		// burn the OTT.
		http.Error(w, "method not allowed; use POST", http.StatusMethodNotAllowed)
		return
	}
	ott := strings.TrimPrefix(r.URL.Path, "/e/")
	if !strings.HasPrefix(ott, "ott_") {
		http.Error(w, "bad token", http.StatusBadRequest)
		return
	}
	token, agent, err := s.store.ConsumeEnrollment(r.Context(), ott)
	if err != nil {
		switch err {
		case ErrEnrollmentNotFound:
			http.Error(w, "unknown enrollment token", http.StatusNotFound)
		case ErrEnrollmentExpired:
			http.Error(w, "enrollment token expired", http.StatusGone)
		case ErrEnrollmentConsumed:
			http.Error(w, "enrollment token already used", http.StatusConflict)
		case ErrEnrollmentNameTaken:
			http.Error(w, "agent name already exists; admin must revoke or rename", http.StatusConflict)
		default:
			s.writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, map[string]any{
		"agent_id":   agent.ID,
		"agent_name": agent.Name,
		"token":      token,
		"central_url": publicBaseURL(r),
	})
}

// publicBaseURL infers the URL the request came in on (honoring X-Forwarded-*
// if a reverse proxy set them). Used so the enrollment URL we hand the
// admin matches the URL the employee will use.
func publicBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	return scheme + "://" + host
}

// handleEventDetail — GET /api/events/{id}. Always includes payload.
func (s *Server) handleEventDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/api/events/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	e, err := s.store.GetEvent(r.Context(), id)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	if e == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, EventDTO{
		ID:        e.ID,
		AgentID:   e.AgentID,
		AgentName: e.AgentName,
		AgentTS:   e.AgentTS.UnixNano(),
		ServerTS:  e.ServerTS.UnixNano(),
		SessionID: e.SessionID,
		Upstream:  e.Upstream,
		Direction: e.Direction,
		MsgType:   e.MsgType,
		MsgID:     e.MsgID,
		Method:    e.Method,
		Bytes:     e.Bytes,
		Payload:   string(e.Payload),
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

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

func zeroTime(t interface{ IsZero() bool }) int64 {
	if t == nil || t.IsZero() {
		return 0
	}
	type unixer interface{ UnixNano() int64 }
	if u, ok := t.(unixer); ok {
		return u.UnixNano()
	}
	return 0
}

