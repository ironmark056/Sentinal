package approval

import (
	"context"
	"testing"
)

func TestAutoDecision_SetGetList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// None initially.
	_, ok, err := s.GetAutoDecision(ctx, "x/y")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected no decision initially")
	}

	if err := s.SetAutoDecision(ctx, "sensitive-path/ssh-secrets", StatusApproved, "alice", "trust me"); err != nil {
		t.Fatal(err)
	}
	dec, ok, err := s.GetAutoDecision(ctx, "sensitive-path/ssh-secrets")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected decision to exist")
	}
	if dec != StatusApproved {
		t.Errorf("want approved, got %q", dec)
	}

	if err := s.SetAutoDecision(ctx, "prompt-injection/instruction-override", StatusDenied, "bob", ""); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAutoDecisions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("want 2 entries, got %d", len(list))
	}
}

func TestAutoDecision_UpsertReplaces(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SetAutoDecision(ctx, "rule/foo", StatusApproved, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAutoDecision(ctx, "rule/foo", StatusDenied, "bob", "changed mind"); err != nil {
		t.Fatal(err)
	}
	dec, _, _ := s.GetAutoDecision(ctx, "rule/foo")
	if dec != StatusDenied {
		t.Errorf("upsert: want denied got %q", dec)
	}
	list, _ := s.ListAutoDecisions(ctx)
	if len(list) != 1 {
		t.Errorf("upsert should not duplicate: got %d rows", len(list))
	}
}

func TestAutoDecision_RejectsBadDecision(t *testing.T) {
	s := newTestStore(t)
	err := s.SetAutoDecision(context.Background(), "rule/foo", StatusPending, "alice", "")
	if err == nil {
		t.Fatal("expected error for pending decision")
	}
}

func TestAutoDecision_RejectsEmptyRule(t *testing.T) {
	s := newTestStore(t)
	err := s.SetAutoDecision(context.Background(), "", StatusApproved, "alice", "")
	if err == nil {
		t.Fatal("expected error for empty rule_id")
	}
}

func TestAutoDecision_DeleteIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Delete non-existent — no error.
	if err := s.DeleteAutoDecision(ctx, "never/existed"); err != nil {
		t.Errorf("delete of missing should be no-op, got %v", err)
	}
	// Set, delete, gone.
	_ = s.SetAutoDecision(ctx, "rule/x", StatusApproved, "alice", "")
	if err := s.DeleteAutoDecision(ctx, "rule/x"); err != nil {
		t.Fatal(err)
	}
	_, ok, _ := s.GetAutoDecision(ctx, "rule/x")
	if ok {
		t.Error("expected decision gone after delete")
	}
}
