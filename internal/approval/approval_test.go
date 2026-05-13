package approval

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sampleApproval() Approval {
	return Approval{
		CreatedAt:    time.Now(),
		SessionID:    "sess-1",
		Upstream:     "echo",
		MsgID:        "42",
		Method:       "tools/call",
		ToolName:     "filesystem.read",
		RiskScore:    55,
		FindingsJSON: []byte(`[{"category":"sensitive-path","rule":"ssh-secrets"}]`),
		Payload:      []byte(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{}}`),
	}
}

func TestInsertAndGet(t *testing.T) {
	s := newTestStore(t)
	id, err := s.Insert(context.Background(), sampleApproval())
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("bad id: %d", id)
	}
	a, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusPending {
		t.Errorf("want pending, got %q", a.Status)
	}
	if a.RiskScore != 55 {
		t.Errorf("score lost: %d", a.RiskScore)
	}
}

func TestListPending(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.Insert(ctx, sampleApproval()); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListPending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("want 3 pending, got %d", len(list))
	}
}

func TestResolve_Approve(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, sampleApproval())

	if err := s.Resolve(ctx, id, StatusApproved, "alice"); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Get(ctx, id)
	if a.Status != StatusApproved {
		t.Errorf("want approved, got %q", a.Status)
	}
	if a.ResolvedBy != "alice" {
		t.Errorf("resolved_by lost: %q", a.ResolvedBy)
	}
	if a.ResolvedAt.IsZero() {
		t.Error("resolved_at not set")
	}
}

func TestResolve_DoubleResolveRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, sampleApproval())
	if err := s.Resolve(ctx, id, StatusApproved, "alice"); err != nil {
		t.Fatal(err)
	}
	err := s.Resolve(ctx, id, StatusDenied, "bob")
	if err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Errorf("want already-resolved error, got %v", err)
	}
}

func TestResolve_BadStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, sampleApproval())
	err := s.Resolve(ctx, id, StatusPending, "x")
	if err == nil {
		t.Fatal("expected error resolving to pending")
	}
}

func TestWaitForDecision_Approved(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	id, _ := s.Insert(ctx, sampleApproval())

	// Approve concurrently after a short delay.
	resolved := int32(0)
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = s.Resolve(context.Background(), id, StatusApproved, "test")
		atomic.StoreInt32(&resolved, 1)
	}()

	start := time.Now()
	status, err := s.WaitForDecision(ctx, id, 25*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusApproved {
		t.Errorf("want approved, got %q", status)
	}
	if elapsed > 1*time.Second {
		t.Errorf("wait took too long: %v", elapsed)
	}
}

func TestWaitForDecision_Timeout(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.Insert(context.Background(), sampleApproval())

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	status, err := s.WaitForDecision(ctx, id, 25*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusTimeout {
		t.Errorf("want timeout, got %q", status)
	}
}
