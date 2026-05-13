package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ironmark056/sentinel/internal/approval"
	"github.com/ironmark056/sentinel/internal/audit"
)

// seedAuditDB writes a handful of representative events and returns the path.
func seedAuditDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.db")
	l, err := audit.New(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	events := []audit.Event{
		{TS: now.Add(-3 * time.Second), SessionID: "sess-a", Upstream: "echo",
			Direction: audit.ClientToServer, MsgType: "request", MsgID: "1", Method: "tools/list",
			Payload: []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), Bytes: 44},
		{TS: now.Add(-2 * time.Second), SessionID: "sess-a", Upstream: "echo",
			Direction: audit.ServerToClient, MsgType: "response", MsgID: "1",
			Payload: []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), Bytes: 36},
		{TS: now.Add(-1 * time.Second), SessionID: "sess-a", Upstream: "echo",
			Direction: audit.ClientToServer, MsgType: "request", MsgID: "2", Method: "tools/call",
			Payload: []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x"}}`), Bytes: 70},
		{TS: now, SessionID: "sess-a", Upstream: "echo",
			Direction: audit.ServerToClient, MsgType: "error", MsgID: "2",
			Payload: []byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"blocked by sentinel policy: [prompt-injection/instruction-override]"}}`), Bytes: 130},
	}
	for _, e := range events {
		l.Append(e)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// newTestHTTP constructs a Server, mounts its routes on an httptest.NewServer,
// and registers cleanup so the audit DB is closed before the test temp dir is
// removed (Windows can't unlink an open file).
func newTestHTTP(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	path := seedAuditDB(t)
	s, err := New(Options{Addr: "127.0.0.1:0", AuditPath: path})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.routes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		ts.Close()
		_ = s.Close()
	})
	return s, ts
}

func getJSON(t *testing.T, ts *httptest.Server, path string, out any) int {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return resp.StatusCode
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode %s: %v\nbody: %s", path, err, body)
	}
	return resp.StatusCode
}

func TestStats_ReportsCounts(t *testing.T) {
	_, ts := newTestHTTP(t)

	var stats Stats
	if code := getJSON(t, ts, "/api/stats", &stats); code != 200 {
		t.Fatalf("status=%d", code)
	}
	if stats.Total != 4 {
		t.Errorf("total: want 4 got %d", stats.Total)
	}
	if stats.BlockedCount != 1 {
		t.Errorf("blocked_count: want 1 got %d", stats.BlockedCount)
	}
	if stats.Sessions != 1 {
		t.Errorf("sessions: want 1 got %d", stats.Sessions)
	}
	if len(stats.RecentBlocked) != 1 {
		t.Errorf("recent_blocked: want 1 entry got %d", len(stats.RecentBlocked))
	}
	if stats.AuditDBPath == "" {
		t.Error("audit_db_path empty")
	}
}

func TestEvents_ListsRecentFirst(t *testing.T) {
	_, ts := newTestHTTP(t)

	var evs []EventSummary
	if code := getJSON(t, ts, "/api/events?limit=50", &evs); code != 200 {
		t.Fatalf("status=%d", code)
	}
	if len(evs) != 4 {
		t.Fatalf("count: want 4 got %d", len(evs))
	}
	if evs[0].MsgType != "error" {
		t.Errorf("expected first (most recent) event to be error, got %q", evs[0].MsgType)
	}
}

func TestEvents_Pagination(t *testing.T) {
	_, ts := newTestHTTP(t)

	var page1 []EventSummary
	getJSON(t, ts, "/api/events?limit=2", &page1)
	if len(page1) != 2 {
		t.Fatalf("page1 len: want 2 got %d", len(page1))
	}

	beforeID := page1[len(page1)-1].ID
	var page2 []EventSummary
	getJSON(t, ts, "/api/events?limit=2&before_id="+itoa(beforeID), &page2)
	if len(page2) != 2 {
		t.Fatalf("page2 len: want 2 got %d", len(page2))
	}
	// No overlap between pages.
	if page2[0].ID >= beforeID {
		t.Errorf("page2 first id %d not strictly less than %d", page2[0].ID, beforeID)
	}
}

func TestEventDetail_IncludesPayload(t *testing.T) {
	_, ts := newTestHTTP(t)

	var evs []EventSummary
	getJSON(t, ts, "/api/events?limit=50", &evs)
	if len(evs) == 0 {
		t.Fatal("no events")
	}

	var ev EventDetail
	if code := getJSON(t, ts, "/api/event/"+itoa(evs[0].ID), &ev); code != 200 {
		t.Fatalf("status=%d", code)
	}
	if ev.Payload == "" {
		t.Error("payload empty")
	}
	if !strings.Contains(ev.Payload, "jsonrpc") {
		t.Errorf("payload missing jsonrpc: %q", ev.Payload)
	}
}

func TestEventDetail_BadID(t *testing.T) {
	_, ts := newTestHTTP(t)

	resp, err := http.Get(ts.URL + "/api/event/not-a-number")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestIndexPage_Served(t *testing.T) {
	_, ts := newTestHTTP(t)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Sentinel") {
		t.Error("index.html should mention Sentinel")
	}
}

func TestStaticAssets_Served(t *testing.T) {
	_, ts := newTestHTTP(t)

	for _, p := range []string{"/app.js", "/style.css"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: status %d", p, resp.StatusCode)
		}
	}
}

func TestApprovals_ListAndResolve(t *testing.T) {
	s, ts := newTestHTTP(t)

	// Insert a pending approval directly via the store.
	id, err := s.approvals.Insert(context.Background(), approval.Approval{
		CreatedAt:    time.Now(),
		SessionID:    "sess-test",
		Upstream:     "echo",
		MsgID:        "99",
		Method:       "tools/call",
		ToolName:     "filesystem.read",
		RiskScore:    50,
		FindingsJSON: []byte(`[{"category":"sensitive-path","rule":"ssh-secrets"}]`),
		Payload:      []byte(`{"jsonrpc":"2.0","id":99,"method":"tools/call"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	// GET /api/approvals
	var list []PendingApproval
	if code := getJSON(t, ts, "/api/approvals", &list); code != 200 {
		t.Fatalf("status=%d", code)
	}
	found := false
	for _, a := range list {
		if a.ID == id {
			found = true
			if a.RiskScore != 50 {
				t.Errorf("risk_score: want 50 got %d", a.RiskScore)
			}
			if a.ToolName != "filesystem.read" {
				t.Errorf("tool_name: %q", a.ToolName)
			}
		}
	}
	if !found {
		t.Fatalf("inserted approval id=%d not in list", id)
	}

	// POST /api/approvals/{id}/approve
	resp, err := http.Post(ts.URL+"/api/approvals/"+itoa(id)+"/approve", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("approve status: %d", resp.StatusCode)
	}

	// Verify status is now approved.
	a, err := s.approvals.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != approval.StatusApproved {
		t.Errorf("want approved, got %q", a.Status)
	}

	// Second resolve attempt must 409.
	resp2, err := http.Post(ts.URL+"/api/approvals/"+itoa(id)+"/deny", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("double resolve: want 409 got %d", resp2.StatusCode)
	}
}

func TestApprovals_BadAction(t *testing.T) {
	_, ts := newTestHTTP(t)
	resp, err := http.Post(ts.URL+"/api/approvals/1/maybe", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}

func TestAuditDBMissing_ReturnsError(t *testing.T) {
	_, err := New(Options{Addr: "127.0.0.1:0", AuditPath: filepath.Join(t.TempDir(), "no-such.db")})
	if err == nil {
		t.Fatal("expected error when audit DB does not exist")
	}
}

func itoa(n int64) string {
	return strings.TrimLeft(string(itoaBytes(n)), "")
}

func itoaBytes(n int64) []byte {
	if n == 0 {
		return []byte("0")
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
	return b[i:]
}
