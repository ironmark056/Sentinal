package centralpolicy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Merge — pure-function semantics
// ---------------------------------------------------------------------------

func TestMerge_Nil(t *testing.T) {
	dp, a, b := Merge(nil, []string{"~/.ssh"}, 30, 80)
	if len(dp) != 1 || dp[0] != "~/.ssh" {
		t.Errorf("nil central should pass through local deny: %v", dp)
	}
	if a != 30 || b != 80 {
		t.Errorf("nil central should pass through thresholds: %d %d", a, b)
	}
}

func TestMerge_UnionsDenyPaths(t *testing.T) {
	central := &Effective{DenyPaths: []string{"~/.aws", "~/.ssh"}}
	dp, _, _ := Merge(central, []string{"~/.ssh", "~/.kube"}, 0, 0)
	got := strings.Join(dp, ",")
	// Local first, then central, dedup; expected: ~/.ssh,~/.kube,~/.aws
	if got != "~/.ssh,~/.kube,~/.aws" {
		t.Errorf("union order wrong: %q", got)
	}
}

func TestMerge_StricterThresholdWins(t *testing.T) {
	cases := []struct {
		localA, localB, centralA, centralB, wantA, wantB int
	}{
		{30, 80, 20, 70, 20, 70}, // central stricter
		{20, 70, 30, 80, 20, 70}, // local stricter
		{0, 80, 25, 0, 25, 80},   // each fills the other's missing
		{0, 0, 25, 75, 25, 75},   // local has nothing → central wins
	}
	for _, c := range cases {
		central := &Effective{ApproveThreshold: c.centralA, BlockThreshold: c.centralB}
		_, a, b := Merge(central, nil, c.localA, c.localB)
		if a != c.wantA || b != c.wantB {
			t.Errorf("Merge(local=%d/%d, central=%d/%d) = %d/%d, want %d/%d",
				c.localA, c.localB, c.centralA, c.centralB, a, b, c.wantA, c.wantB)
		}
	}
}

// ---------------------------------------------------------------------------
// Fetch round-trip against a stub central server
// ---------------------------------------------------------------------------

func TestFetcher_Refresh_Success(t *testing.T) {
	var calls int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/agent/v1/policy" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer mcpg_test" {
			t.Errorf("missing bearer")
		}
		w.Header().Set("ETag", "etag-v1")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   1,
			"etag": "etag-v1",
			"body": map[string]any{
				"deny_paths": []string{"~/.aws"},
				"scoring":    map[string]any{"approve_threshold": 25, "block_threshold": 70},
			},
		})
	}))
	defer stub.Close()

	dir := t.TempDir()
	f, err := New(Options{
		URL:       stub.URL,
		Token:     "mcpg_test",
		CachePath: filepath.Join(dir, "central-policy.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := f.Refresh(context.Background())
	if got == nil {
		t.Fatal("nil effective")
	}
	if got.SourceLastFetchFailure != "" {
		t.Errorf("expected no failure, got %q", got.SourceLastFetchFailure)
	}
	if got.SourceETag != "etag-v1" {
		t.Errorf("etag: %q", got.SourceETag)
	}
	if len(got.DenyPaths) != 1 || got.DenyPaths[0] != "~/.aws" {
		t.Errorf("deny: %v", got.DenyPaths)
	}
	if got.ApproveThreshold != 25 || got.BlockThreshold != 70 {
		t.Errorf("thresholds: %d/%d", got.ApproveThreshold, got.BlockThreshold)
	}
}

func TestFetcher_Refresh_FailoverToCache(t *testing.T) {
	// First, prime the cache with a successful fetch against one stub.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", "etag-warm")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   1,
			"etag": "etag-warm",
			"body": map[string]any{"deny_paths": []string{"~/.cached"}},
		})
	}))
	dir := t.TempDir()
	cache := filepath.Join(dir, "central-policy.json")

	f, _ := New(Options{URL: primary.URL, Token: "t", CachePath: cache})
	if got := f.Refresh(context.Background()); got.SourceLastFetchFailure != "" {
		t.Fatalf("warm-up fetch failed: %s", got.SourceLastFetchFailure)
	}
	primary.Close() // simulate central going away

	// Build a new fetcher pointing at the same dead URL; Refresh should
	// fail the network call but successfully load from cache.
	f2, _ := New(Options{URL: primary.URL, Token: "t", CachePath: cache})
	got := f2.Refresh(context.Background())
	if got == nil {
		t.Fatal("nil")
	}
	if got.SourceLastFetchFailure == "" {
		t.Errorf("expected failure note set")
	}
	if len(got.DenyPaths) != 1 || got.DenyPaths[0] != "~/.cached" {
		t.Errorf("cache not loaded: %+v", got)
	}
}

func TestFetcher_Refresh_FailNoCache(t *testing.T) {
	// Point at a never-listening port; force a quick failure.
	f, _ := New(Options{
		URL:       "http://127.0.0.1:1", // RFC says port 1 is unassigned; will be ECONNREFUSED on most systems.
		Token:     "t",
		CachePath: filepath.Join(t.TempDir(), "central-policy.json"),
		Client:    &http.Client{Timeout: 200 * time.Millisecond},
	})
	got := f.Refresh(context.Background())
	if got == nil {
		t.Fatal("nil")
	}
	if !got.Empty {
		t.Errorf("expected Empty=true on no-cache no-server, got %+v", got)
	}
	if got.SourceLastFetchFailure == "" {
		t.Error("expected failure note")
	}
}

func TestFetcher_HonorsIfNoneMatch_OnRefresh(t *testing.T) {
	var serverETag = "etag-v1"
	var bodyCalls int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == serverETag {
			w.Header().Set("ETag", serverETag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		atomic.AddInt32(&bodyCalls, 1)
		w.Header().Set("ETag", serverETag)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   1,
			"etag": serverETag,
			"body": map[string]any{"deny_paths": []string{"~/.aws"}},
		})
	}))
	defer stub.Close()

	f, _ := New(Options{
		URL:       stub.URL,
		Token:     "t",
		CachePath: filepath.Join(t.TempDir(), "central-policy.json"),
	})
	first := f.Refresh(context.Background())
	if first.SourceETag != serverETag {
		t.Fatalf("first etag: %q", first.SourceETag)
	}

	second := f.Refresh(context.Background())
	if second.SourceETag != serverETag {
		t.Errorf("second etag changed: %q", second.SourceETag)
	}
	if len(second.DenyPaths) != 1 {
		t.Errorf("304 path should retain deny paths: %v", second.DenyPaths)
	}
	if atomic.LoadInt32(&bodyCalls) != 1 {
		t.Errorf("server should only have served the body once; called %d times", bodyCalls)
	}
}

func TestFetcher_RejectsBadOptions(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("empty options should error")
	}
	if _, err := New(Options{URL: "http://x"}); err == nil {
		t.Error("missing token should error")
	}
}
