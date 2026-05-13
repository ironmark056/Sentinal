package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(Options{Addr: "127.0.0.1:0", DataDir: dir, AdminToken: "admin-secret"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.routes(mux)
	ts := httptest.NewServer(s.withMiddleware(mux))
	t.Cleanup(func() {
		ts.Close()
		_ = s.Close()
	})
	return s, ts
}

// ---------------------------------------------------------------------------
// Open endpoints
// ---------------------------------------------------------------------------

func TestHealthEndpoint(t *testing.T) {
	_, ts := newTestServer(t)
	r, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Errorf("status: %d", r.StatusCode)
	}
}

func TestVersionEndpoint(t *testing.T) {
	_, ts := newTestServer(t)
	r, _ := http.Get(ts.URL + "/version")
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(body), "version") {
		t.Errorf("missing version key in body: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Agent management
// ---------------------------------------------------------------------------

func TestCreateAgent_GeneratesTokenAndStoresHash(t *testing.T) {
	s, _ := newTestServer(t)
	token, agent, err := s.store.CreateAgent(context.Background(), "alice-laptop",
		map[string]string{"os": "macos"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "mcpg_") {
		t.Errorf("token prefix: %q", token)
	}
	if len(token) < 32 {
		t.Errorf("token too short: %d", len(token))
	}
	if agent.Name != "alice-laptop" {
		t.Errorf("name: %q", agent.Name)
	}

	// Looking up by the raw token works.
	got, err := s.store.LookupAgentByToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != agent.ID {
		t.Errorf("lookup failed: %+v", got)
	}

	// Lookup by a different token fails (nil, nil).
	bad, err := s.store.LookupAgentByToken(context.Background(), "mcpg_bogus")
	if err != nil {
		t.Fatal(err)
	}
	if bad != nil {
		t.Errorf("bogus token should not match: %+v", bad)
	}
}

func TestCreateAgent_RejectsDuplicateName(t *testing.T) {
	s, _ := newTestServer(t)
	_, _, err := s.store.CreateAgent(context.Background(), "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.store.CreateAgent(context.Background(), "alice", nil)
	if err == nil {
		t.Error("expected error on duplicate name")
	}
}

// ---------------------------------------------------------------------------
// Auth — agent endpoints require bearer
// ---------------------------------------------------------------------------

func TestAgentEndpoint_RejectsMissingToken(t *testing.T) {
	_, ts := newTestServer(t)
	r, _ := http.Post(ts.URL+"/agent/v1/events", "application/json",
		strings.NewReader(`{"events":[]}`))
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestAgentEndpoint_RejectsBadToken(t *testing.T) {
	_, ts := newTestServer(t)
	req, _ := http.NewRequest("POST", ts.URL+"/agent/v1/events",
		strings.NewReader(`{"events":[]}`))
	req.Header.Set("Authorization", "Bearer mcpg_invalid")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", r.StatusCode)
	}
}

func TestAgentEndpoint_AcceptsValidToken(t *testing.T) {
	s, ts := newTestServer(t)
	token, _, _ := s.store.CreateAgent(context.Background(), "alice", nil)

	req, _ := http.NewRequest("GET", ts.URL+"/agent/v1/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Errorf("want 200, got %d", r.StatusCode)
	}
	body, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(body), "alice") {
		t.Errorf("body should echo agent name: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Event ingest
// ---------------------------------------------------------------------------

func TestIngestEvents_RoundTrip(t *testing.T) {
	s, ts := newTestServer(t)
	token, agent, _ := s.store.CreateAgent(context.Background(), "alice", nil)

	body := IngestRequest{Events: []IngestEvent{
		{
			AgentTS:   time.Now().UnixNano(),
			SessionID: "sess-1",
			Upstream:  "echo",
			Direction: "c2s",
			MsgType:   "request",
			MsgID:     "1",
			Method:    "tools/list",
			Payload:   json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
			Bytes:     44,
		},
		{
			AgentTS:   time.Now().UnixNano(),
			SessionID: "sess-1",
			Upstream:  "echo",
			Direction: "s2c",
			MsgType:   "response",
			MsgID:     "1",
			Payload:   json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`),
			Bytes:     36,
		},
	}}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", ts.URL+"/agent/v1/events", bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("ingest status: %d", r.StatusCode)
	}
	var resp IngestResponse
	_ = json.NewDecoder(r.Body).Decode(&resp)
	if resp.Accepted != 2 {
		t.Errorf("accepted: want 2 got %d", resp.Accepted)
	}

	// Confirm both events landed and are joined to the right agent.
	list, err := s.store.ListEvents(context.Background(), EventFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list len: %d", len(list))
	}
	for _, e := range list {
		if e.AgentID != agent.ID {
			t.Errorf("agent_id: %d", e.AgentID)
		}
		if e.AgentName != "alice" {
			t.Errorf("agent_name: %q", e.AgentName)
		}
	}
}

func TestIngest_SkipsMalformed(t *testing.T) {
	s, ts := newTestServer(t)
	token, _, _ := s.store.CreateAgent(context.Background(), "alice", nil)

	body := IngestRequest{Events: []IngestEvent{
		{ // missing required fields
			Payload: json.RawMessage(`{}`),
		},
		{
			AgentTS:   time.Now().UnixNano(),
			SessionID: "sess-1",
			Upstream:  "echo",
			Direction: "c2s",
			MsgType:   "request",
			Payload:   json.RawMessage(`{}`),
		},
	}}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", ts.URL+"/agent/v1/events", bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+token)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()

	var resp IngestResponse
	_ = json.NewDecoder(r.Body).Decode(&resp)
	if resp.Accepted != 1 {
		t.Errorf("want 1 accepted (malformed dropped), got %d", resp.Accepted)
	}
}

// ---------------------------------------------------------------------------
// Dashboard API
// ---------------------------------------------------------------------------

func TestStats_AggregatesAcrossAgents(t *testing.T) {
	s, ts := newTestServer(t)
	tokenA, _, _ := s.store.CreateAgent(context.Background(), "alice", nil)
	tokenB, _, _ := s.store.CreateAgent(context.Background(), "bob", nil)

	for _, token := range []string{tokenA, tokenB} {
		body, _ := json.Marshal(IngestRequest{Events: []IngestEvent{{
			AgentTS:   time.Now().UnixNano(),
			SessionID: "s",
			Upstream:  "x",
			Direction: "c2s",
			MsgType:   "request",
			Payload:   json.RawMessage(`{}`),
		}}})
		req, _ := http.NewRequest("POST", ts.URL+"/agent/v1/events", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		_, _ = http.DefaultClient.Do(req)
	}

	r, _ := http.Get(ts.URL + "/api/stats")
	var st StatsDTO
	_ = json.NewDecoder(r.Body).Decode(&st)
	r.Body.Close()
	if st.Total != 2 {
		t.Errorf("total: want 2, got %d", st.Total)
	}
	if st.AgentCount != 2 {
		t.Errorf("agent_count: want 2, got %d", st.AgentCount)
	}
}

func TestAgents_ListEndpoint(t *testing.T) {
	s, ts := newTestServer(t)
	_, _, _ = s.store.CreateAgent(context.Background(), "alice", nil)
	_, _, _ = s.store.CreateAgent(context.Background(), "bob", nil)

	r, err := http.Get(ts.URL + "/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var list []AgentDTO
	_ = json.NewDecoder(r.Body).Decode(&list)
	if len(list) != 2 {
		t.Errorf("len: %d", len(list))
	}
}

func TestAgents_CreateRequiresAdminToken(t *testing.T) {
	_, ts := newTestServer(t)
	// Without admin token → 401.
	body := strings.NewReader(`{"name":"alice"}`)
	req, _ := http.NewRequest("POST", ts.URL+"/api/agents", body)
	req.Header.Set("Content-Type", "application/json")
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", r.StatusCode)
	}
	r.Body.Close()

	// With admin token → 200 + token in body.
	body = strings.NewReader(`{"name":"alice"}`)
	req, _ = http.NewRequest("POST", ts.URL+"/api/agents", body)
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("Content-Type", "application/json")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("admin create: %d", r2.StatusCode)
	}
	rb, _ := io.ReadAll(r2.Body)
	if !strings.Contains(string(rb), "mcpg_") {
		t.Errorf("response should contain token: %s", rb)
	}
}

func TestEvents_FilterByAgent(t *testing.T) {
	s, ts := newTestServer(t)
	tokenA, agentA, _ := s.store.CreateAgent(context.Background(), "alice", nil)
	tokenB, _, _ := s.store.CreateAgent(context.Background(), "bob", nil)

	for _, token := range []string{tokenA, tokenA, tokenB} {
		body, _ := json.Marshal(IngestRequest{Events: []IngestEvent{{
			AgentTS:   time.Now().UnixNano(),
			SessionID: "s",
			Upstream:  "x",
			Direction: "c2s",
			MsgType:   "request",
			Payload:   json.RawMessage(`{}`),
		}}})
		req, _ := http.NewRequest("POST", ts.URL+"/agent/v1/events", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		_, _ = http.DefaultClient.Do(req)
	}

	r, err := http.Get(ts.URL + "/api/events?agent_id=" + itoa(agentA.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var list []EventDTO
	_ = json.NewDecoder(r.Body).Decode(&list)
	if len(list) != 2 {
		t.Errorf("alice events: want 2, got %d", len(list))
	}
	for _, e := range list {
		if e.AgentName != "alice" {
			t.Errorf("wrong agent: %q", e.AgentName)
		}
	}
}

// ---------------------------------------------------------------------------
// Single-event detail endpoint
// ---------------------------------------------------------------------------

func TestEventDetail_ReturnsPayloadAndJoinedAgent(t *testing.T) {
	s, ts := newTestServer(t)
	token, agent, _ := s.store.CreateAgent(context.Background(), "alice", nil)

	body, _ := json.Marshal(IngestRequest{Events: []IngestEvent{{
		AgentTS:   time.Now().UnixNano(),
		SessionID: "sess-detail",
		Upstream:  "echo",
		Direction: "c2s",
		MsgType:   "request",
		MsgID:     "42",
		Method:    "tools/call",
		Payload:   json.RawMessage(`{"jsonrpc":"2.0","id":42,"method":"tools/call"}`),
		Bytes:     50,
	}}})
	req, _ := http.NewRequest("POST", ts.URL+"/agent/v1/events", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	_, _ = http.DefaultClient.Do(req)

	list, _ := s.store.ListEvents(context.Background(), EventFilter{Limit: 10})
	if len(list) != 1 {
		t.Fatalf("expected 1 event in store, got %d", len(list))
	}
	id := list[0].ID

	r, err := http.Get(ts.URL + "/api/events/" + itoa(id))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("status: %d", r.StatusCode)
	}
	var got EventDTO
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ID != id {
		t.Errorf("id: want %d got %d", id, got.ID)
	}
	if got.AgentName != "alice" {
		t.Errorf("agent_name: %q", got.AgentName)
	}
	if got.AgentID != agent.ID {
		t.Errorf("agent_id: %d", got.AgentID)
	}
	if got.Method != "tools/call" {
		t.Errorf("method: %q", got.Method)
	}
	if !strings.Contains(got.Payload, "tools/call") {
		t.Errorf("payload should be included: %q", got.Payload)
	}
}

func TestEventDetail_NotFound(t *testing.T) {
	_, ts := newTestServer(t)
	r, err := http.Get(ts.URL + "/api/events/99999")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", r.StatusCode)
	}
}

func TestEventDetail_BadID(t *testing.T) {
	_, ts := newTestServer(t)
	r, err := http.Get(ts.URL + "/api/events/not-a-number")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Sessions (slice 0.2.3)
// ---------------------------------------------------------------------------

// seedSession injects N events under the given (token, sessionID) so we
// can assert ListSessions / /api/sessions group + count correctly.
func seedSession(t *testing.T, ts *httptest.Server, token, sessionID string, blocked bool, methods ...string) {
	t.Helper()
	events := make([]IngestEvent, 0, len(methods))
	for _, m := range methods {
		ev := IngestEvent{
			AgentTS:   time.Now().UnixNano(),
			SessionID: sessionID,
			Upstream:  "echo",
			Direction: "c2s",
			MsgType:   "request",
			Method:    m,
			Payload:   json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"` + m + `"}`),
		}
		events = append(events, ev)
	}
	if blocked {
		events = append(events, IngestEvent{
			AgentTS:   time.Now().UnixNano(),
			SessionID: sessionID,
			Upstream:  "echo",
			Direction: "s2c",
			MsgType:   "error",
			Payload:   json.RawMessage(`{"error":{"message":"blocked by sentinel policy"}}`),
		})
	}
	body, _ := json.Marshal(IngestRequest{Events: events})
	req, _ := http.NewRequest("POST", ts.URL+"/agent/v1/events", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
}

func TestSessions_AggregatesAcrossEvents(t *testing.T) {
	s, ts := newTestServer(t)
	tokenA, agentA, _ := s.store.CreateAgent(context.Background(), "alice", nil)
	tokenB, _, _ := s.store.CreateAgent(context.Background(), "bob", nil)

	// Alice has two sessions, bob has one. One of alice's sessions has a block.
	seedSession(t, ts, tokenA, "alice-sess-1", true, "tools/list", "tools/call")
	seedSession(t, ts, tokenA, "alice-sess-2", false, "tools/list")
	seedSession(t, ts, tokenB, "bob-sess-1", false, "tools/list", "tools/list", "tools/list")

	// Fleet-wide.
	r, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var list []SessionDTO
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 sessions, got %d", len(list))
	}

	byID := map[string]SessionDTO{}
	for _, sRow := range list {
		byID[sRow.SessionID] = sRow
	}
	if got := byID["alice-sess-1"]; got.BlockedCount != 1 {
		t.Errorf("alice-sess-1 blocked: want 1, got %d", got.BlockedCount)
	}
	if got := byID["alice-sess-1"]; got.EventCount != 3 { // 2 requests + 1 error
		t.Errorf("alice-sess-1 events: want 3, got %d", got.EventCount)
	}
	if got := byID["bob-sess-1"]; got.EventCount != 3 {
		t.Errorf("bob-sess-1 events: want 3, got %d", got.EventCount)
	}
	if got := byID["bob-sess-1"]; got.AgentName != "bob" {
		t.Errorf("bob-sess-1 agent: %q", got.AgentName)
	}
	if got := byID["alice-sess-1"]; !strings.Contains(got.Upstreams, "echo") {
		t.Errorf("upstreams should include 'echo': %q", got.Upstreams)
	}

	// Filtered to alice.
	r2, err := http.Get(ts.URL + "/api/sessions?agent_id=" + itoa(agentA.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var aliceOnly []SessionDTO
	_ = json.NewDecoder(r2.Body).Decode(&aliceOnly)
	if len(aliceOnly) != 2 {
		t.Errorf("alice sessions: want 2, got %d", len(aliceOnly))
	}
	for _, sRow := range aliceOnly {
		if sRow.AgentName != "alice" {
			t.Errorf("filter leaked %q", sRow.AgentName)
		}
	}
}

func TestEvents_FilterBySession(t *testing.T) {
	s, ts := newTestServer(t)
	tokenA, _, _ := s.store.CreateAgent(context.Background(), "alice", nil)

	seedSession(t, ts, tokenA, "s-one", false, "tools/list", "tools/call")
	seedSession(t, ts, tokenA, "s-two", false, "ping")

	r, err := http.Get(ts.URL + "/api/events?session_id=s-one")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var list []EventDTO
	_ = json.NewDecoder(r.Body).Decode(&list)
	if len(list) != 2 {
		t.Errorf("session_id filter: want 2, got %d", len(list))
	}
	for _, e := range list {
		if e.SessionID != "s-one" {
			t.Errorf("leaked session %q", e.SessionID)
		}
	}
}

// ---------------------------------------------------------------------------
// Payload search (slice 0.2.4)
// ---------------------------------------------------------------------------

// seedEventWithPayload injects one event with a specific payload string.
func seedEventWithPayload(t *testing.T, ts *httptest.Server, token, payload string) {
	t.Helper()
	body, _ := json.Marshal(IngestRequest{Events: []IngestEvent{{
		AgentTS:   time.Now().UnixNano(),
		SessionID: "sess-q",
		Upstream:  "echo",
		Direction: "c2s",
		MsgType:   "request",
		Method:    "tools/call",
		Payload:   json.RawMessage(payload),
	}}})
	req, _ := http.NewRequest("POST", ts.URL+"/agent/v1/events", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
}

func TestEvents_FilterByQuery(t *testing.T) {
	s, ts := newTestServer(t)
	tokenA, _, _ := s.store.CreateAgent(context.Background(), "alice", nil)

	seedEventWithPayload(t, ts, tokenA, `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"ssh"}}`)
	seedEventWithPayload(t, ts, tokenA, `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"read_file"}}`)
	seedEventWithPayload(t, ts, tokenA, `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"web_search"}}`)

	r, err := http.Get(ts.URL + "/api/events?q=ssh")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var list []EventDTO
	_ = json.NewDecoder(r.Body).Decode(&list)
	if len(list) != 1 {
		t.Errorf("q=ssh: want 1 match, got %d", len(list))
	}
}

func TestEvents_QueryNoMatch(t *testing.T) {
	s, ts := newTestServer(t)
	tokenA, _, _ := s.store.CreateAgent(context.Background(), "alice", nil)
	seedEventWithPayload(t, ts, tokenA, `{"method":"ping"}`)

	r, err := http.Get(ts.URL + "/api/events?q=nonexistent_token")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var list []EventDTO
	_ = json.NewDecoder(r.Body).Decode(&list)
	if len(list) != 0 {
		t.Errorf("q=nonexistent_token: want 0, got %d", len(list))
	}
}

func TestEvents_EmptyQuery_NoFilter(t *testing.T) {
	s, ts := newTestServer(t)
	tokenA, _, _ := s.store.CreateAgent(context.Background(), "alice", nil)
	seedEventWithPayload(t, ts, tokenA, `{"a":1}`)
	seedEventWithPayload(t, ts, tokenA, `{"a":2}`)

	r, err := http.Get(ts.URL + "/api/events?q=")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var list []EventDTO
	_ = json.NewDecoder(r.Body).Decode(&list)
	if len(list) != 2 {
		t.Errorf("empty q: want 2, got %d", len(list))
	}
}

func TestEvents_QueryComposesWithAgent(t *testing.T) {
	s, ts := newTestServer(t)
	tokenA, agentA, _ := s.store.CreateAgent(context.Background(), "alice", nil)
	tokenB, _, _ := s.store.CreateAgent(context.Background(), "bob", nil)

	// Both agents have an event containing the same needle.
	seedEventWithPayload(t, ts, tokenA, `{"method":"tools/call","params":{"name":"ssh"}}`)
	seedEventWithPayload(t, ts, tokenB, `{"method":"tools/call","params":{"name":"ssh"}}`)

	r, err := http.Get(ts.URL + "/api/events?q=ssh&agent_id=" + itoa(agentA.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var list []EventDTO
	_ = json.NewDecoder(r.Body).Decode(&list)
	if len(list) != 1 {
		t.Fatalf("q+agent: want 1, got %d", len(list))
	}
	if list[0].AgentName != "alice" {
		t.Errorf("filter leaked: %q", list[0].AgentName)
	}
}

func TestEvents_QueryComposesWithSession(t *testing.T) {
	s, ts := newTestServer(t)
	tokenA, _, _ := s.store.CreateAgent(context.Background(), "alice", nil)

	// Two different sessions both contain the needle "ssh".
	seedSession(t, ts, tokenA, "s-A", false, "tools/call_ssh_A1", "tools/call_ssh_A2")
	seedSession(t, ts, tokenA, "s-B", false, "tools/call_ssh_B1")

	r, err := http.Get(ts.URL + "/api/events?q=ssh&session_id=s-B")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var list []EventDTO
	_ = json.NewDecoder(r.Body).Decode(&list)
	if len(list) != 1 {
		t.Errorf("q+session: want 1, got %d", len(list))
	}
	for _, e := range list {
		if e.SessionID != "s-B" {
			t.Errorf("session leak: %q", e.SessionID)
		}
	}
}

// ---------------------------------------------------------------------------
// Central policy (slice 0.2.7)
// ---------------------------------------------------------------------------

func TestPolicy_EmptyServerReturnsEmptyBody(t *testing.T) {
	_, ts := newTestServer(t)
	r, err := http.Get(ts.URL + "/api/policy")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("status: %d", r.StatusCode)
	}
	var got PolicyDTO
	_ = json.NewDecoder(r.Body).Decode(&got)
	if got.ID != 0 || string(got.Body) != "{}" {
		t.Errorf("empty policy: %+v", got)
	}
}

func TestPolicy_PutRequiresAdmin(t *testing.T) {
	_, ts := newTestServer(t)
	body := strings.NewReader(`{"deny_paths":["~/.ssh"]}`)
	req, _ := http.NewRequest("PUT", ts.URL+"/api/policy", body)
	req.Header.Set("Content-Type", "application/json")
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", r.StatusCode)
	}
}

func TestPolicy_PutThenGet(t *testing.T) {
	_, ts := newTestServer(t)

	body := strings.NewReader(`{"deny_paths":["~/.ssh","~/.aws"],"scoring":{"approve_threshold":20,"block_threshold":75}}`)
	req, _ := http.NewRequest("PUT", ts.URL+"/api/policy", body)
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("put: %d body=%s", r.StatusCode, body)
	}
	etag := r.Header.Get("ETag")
	if etag == "" {
		t.Error("PUT should set ETag")
	}

	// GET should return what we just PUT, with the same ETag.
	r2, err := http.Get(ts.URL + "/api/policy")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.Header.Get("ETag") != etag {
		t.Errorf("GET etag %q != PUT etag %q", r2.Header.Get("ETag"), etag)
	}
	var got PolicyDTO
	_ = json.NewDecoder(r2.Body).Decode(&got)
	if !strings.Contains(string(got.Body), "~/.ssh") {
		t.Errorf("GET body missing deny path: %s", got.Body)
	}
	if !strings.Contains(string(got.Body), "approve_threshold") {
		t.Errorf("GET body missing scoring: %s", got.Body)
	}
}

func TestPolicy_PutRejectsInvalidJSON(t *testing.T) {
	_, ts := newTestServer(t)
	req, _ := http.NewRequest("PUT", ts.URL+"/api/policy",
		strings.NewReader("not json at all"))
	req.Header.Set("Authorization", "Bearer admin-secret")
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.StatusCode)
	}
}

func TestPolicy_AgentEndpointRequiresBearer(t *testing.T) {
	_, ts := newTestServer(t)
	r, _ := http.Get(ts.URL + "/agent/v1/policy")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", r.StatusCode)
	}
}

func TestPolicy_AgentEndpointReturnsBodyAndHonorsIfNoneMatch(t *testing.T) {
	s, ts := newTestServer(t)
	token, _, _ := s.store.CreateAgent(context.Background(), "alice", nil)

	// Set a policy.
	pBody := strings.NewReader(`{"deny_paths":["~/.ssh"]}`)
	putReq, _ := http.NewRequest("PUT", ts.URL+"/api/policy", pBody)
	putReq.Header.Set("Authorization", "Bearer admin-secret")
	r, _ := http.DefaultClient.Do(putReq)
	r.Body.Close()
	etag := r.Header.Get("ETag")

	// First agent fetch — no If-None-Match → 200.
	req, _ := http.NewRequest("GET", ts.URL+"/agent/v1/policy", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("agent fetch: %d", r2.StatusCode)
	}
	if r2.Header.Get("ETag") != etag {
		t.Errorf("etag mismatch")
	}

	// Second fetch with If-None-Match → 304.
	req3, _ := http.NewRequest("GET", ts.URL+"/agent/v1/policy", nil)
	req3.Header.Set("Authorization", "Bearer "+token)
	req3.Header.Set("If-None-Match", etag)
	r3, _ := http.DefaultClient.Do(req3)
	r3.Body.Close()
	if r3.StatusCode != http.StatusNotModified {
		t.Errorf("want 304 on If-None-Match match, got %d", r3.StatusCode)
	}
}

func TestPolicy_RevisionsList(t *testing.T) {
	_, ts := newTestServer(t)

	for i, body := range []string{
		`{"deny_paths":["~/.ssh"]}`,
		`{"deny_paths":["~/.ssh","~/.aws"]}`,
		`{"deny_paths":["~/.ssh","~/.aws","~/.kube"]}`,
	} {
		req, _ := http.NewRequest("PUT", ts.URL+"/api/policy", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-secret")
		r, _ := http.DefaultClient.Do(req)
		r.Body.Close()
		_ = i
	}

	r, err := http.Get(ts.URL + "/api/policy/revisions")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var list []PolicyDTO
	_ = json.NewDecoder(r.Body).Decode(&list)
	if len(list) != 3 {
		t.Errorf("want 3 revisions, got %d", len(list))
	}
	// Newest first.
	if !strings.Contains(string(list[0].Body), "~/.kube") {
		t.Errorf("expected newest revision first; got body: %s", list[0].Body)
	}
}

// ---------------------------------------------------------------------------
// Enrollments (slice 0.2.5)
// ---------------------------------------------------------------------------

func TestEnrollment_CreateThenConsume(t *testing.T) {
	s, ts := newTestServer(t)
	ott, en, err := s.store.CreateEnrollment(context.Background(), "alice-laptop", time.Hour,
		map[string]string{"owner": "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ott, "ott_") {
		t.Errorf("ott prefix: %q", ott)
	}
	if en.Name != "alice-laptop" {
		t.Errorf("name: %q", en.Name)
	}

	// Consume over HTTP — public endpoint, no auth.
	req, _ := http.NewRequest("POST", ts.URL+"/e/"+ott, nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("consume: %d body=%s", r.StatusCode, body)
	}
	var resp struct {
		AgentID    int64  `json:"agent_id"`
		AgentName  string `json:"agent_name"`
		Token      string `json:"token"`
		CentralURL string `json:"central_url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&resp)
	if !strings.HasPrefix(resp.Token, "mcpg_") {
		t.Errorf("token prefix: %q", resp.Token)
	}
	if resp.AgentName != "alice-laptop" {
		t.Errorf("agent_name: %q", resp.AgentName)
	}
	if resp.CentralURL == "" {
		t.Errorf("central_url empty")
	}

	// The returned bearer token should authenticate as the agent.
	req2, _ := http.NewRequest("GET", ts.URL+"/agent/v1/health", nil)
	req2.Header.Set("Authorization", "Bearer "+resp.Token)
	r2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Errorf("agent auth via enrollment token: %d", r2.StatusCode)
	}
}

func TestEnrollment_ConsumeTwiceFails(t *testing.T) {
	s, ts := newTestServer(t)
	ott, _, _ := s.store.CreateEnrollment(context.Background(), "alice", time.Hour, nil)

	req, _ := http.NewRequest("POST", ts.URL+"/e/"+ott, nil)
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("first consume: %d", r.StatusCode)
	}

	req2, _ := http.NewRequest("POST", ts.URL+"/e/"+ott, nil)
	r2, _ := http.DefaultClient.Do(req2)
	r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Errorf("second consume: want 409, got %d", r2.StatusCode)
	}
}

func TestEnrollment_ExpiredFails(t *testing.T) {
	s, ts := newTestServer(t)
	// Create with negative TTL... CreateEnrollment clamps to 24h on <=0,
	// so we cheat and set expires_at directly via the same DB.
	ott, en, _ := s.store.CreateEnrollment(context.Background(), "alice", time.Hour, nil)
	// Force expiry in the past.
	_, err := s.store.DB().ExecContext(context.Background(),
		`UPDATE enrollments SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Minute).UnixNano(), en.ID)
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("POST", ts.URL+"/e/"+ott, nil)
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != http.StatusGone {
		t.Errorf("expired consume: want 410, got %d", r.StatusCode)
	}
}

func TestEnrollment_UnknownTokenIs404(t *testing.T) {
	_, ts := newTestServer(t)
	req, _ := http.NewRequest("POST", ts.URL+"/e/ott_doesnotexist", nil)
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", r.StatusCode)
	}
}

func TestEnrollment_NameCollisionIs409(t *testing.T) {
	s, ts := newTestServer(t)
	_, _, _ = s.store.CreateAgent(context.Background(), "alice", nil)

	ott, _, _ := s.store.CreateEnrollment(context.Background(), "alice", time.Hour, nil)
	req, _ := http.NewRequest("POST", ts.URL+"/e/"+ott, nil)
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Errorf("want 409 on name taken, got %d", r.StatusCode)
	}
}

func TestEnrollment_AdminCreateAndRevoke(t *testing.T) {
	_, ts := newTestServer(t)

	// Create via admin endpoint.
	body := strings.NewReader(`{"name":"alice","ttl_seconds":3600}`)
	req, _ := http.NewRequest("POST", ts.URL+"/api/enroll", body)
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("admin create: %d", r.StatusCode)
	}
	var created struct {
		OTT        string `json:"ott"`
		URL        string `json:"url"`
		Enrollment struct {
			ID int64 `json:"id"`
		} `json:"enrollment"`
	}
	_ = json.NewDecoder(r.Body).Decode(&created)
	if created.OTT == "" || created.URL == "" {
		t.Fatalf("admin create response missing fields: %+v", created)
	}

	// Without admin token → 401.
	body = strings.NewReader(`{"name":"bob"}`)
	req, _ = http.NewRequest("POST", ts.URL+"/api/enroll", body)
	r2, _ := http.DefaultClient.Do(req)
	r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Errorf("without admin: want 401, got %d", r2.StatusCode)
	}

	// Revoke.
	req3, _ := http.NewRequest("DELETE",
		ts.URL+"/api/enroll/"+itoa(created.Enrollment.ID), nil)
	req3.Header.Set("Authorization", "Bearer admin-secret")
	r3, _ := http.DefaultClient.Do(req3)
	r3.Body.Close()
	if r3.StatusCode != http.StatusNoContent {
		t.Errorf("revoke: want 204, got %d", r3.StatusCode)
	}

	// Consuming the revoked token now 404s.
	req4, _ := http.NewRequest("POST", ts.URL+"/e/"+created.OTT, nil)
	r4, _ := http.DefaultClient.Do(req4)
	r4.Body.Close()
	if r4.StatusCode != http.StatusNotFound {
		t.Errorf("revoked consume: want 404, got %d", r4.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Embedded SPA assets
// ---------------------------------------------------------------------------

func TestStaticAssets_Served(t *testing.T) {
	_, ts := newTestServer(t)

	tests := []struct {
		path     string
		contains string
		mime     string
	}{
		{"/", "Sentinel Central", "text/html"},
		{"/index.html", "Sentinel Central", "text/html"},
		{"/app.js", "refreshAll", "javascript"},
		{"/style.css", "--bg:", "css"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			r, err := http.Get(ts.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			if r.StatusCode != 200 {
				t.Fatalf("status: %d", r.StatusCode)
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), tc.contains) {
				snippet := string(body)
				if len(snippet) > 200 {
					snippet = snippet[:200]
				}
				t.Errorf("body should contain %q; got: %s", tc.contains, snippet)
			}
			if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), tc.mime) {
				t.Errorf("Content-Type: want substring %q, got %q", tc.mime, r.Header.Get("Content-Type"))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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
