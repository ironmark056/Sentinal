// Package centralpolicy fetches the company-wide policy from a
// sentinel-server and merges it with the local sentinel.yaml policy on
// the agent's side.
//
// Merge semantics — stricter wins, central never weakens local:
//
//	DenyPaths            union (deduplicated)
//	ApproveThreshold     min of the non-zero values
//	BlockThreshold       min of the non-zero values
//
// Central's `enabled` toggle is intentionally not honored — only the
// local install owns the decision to run policy enforcement at all.
//
// Fetch-and-cache model: one Refresh() at startup with a short timeout,
// then a background Run() that re-fetches on a poll interval. If central
// is unreachable, the disk cache is used; if there's no cache either,
// the agent runs with local-only policy. The engine snapshot is built
// once at startup from the merged result; dynamic re-application of a
// changed central policy without a process restart is a follow-up
// (0.2.7.1).
package centralpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Effective is the parsed shape of one central policy snapshot.
type Effective struct {
	DenyPaths              []string  `json:"deny_paths,omitempty"`
	ApproveThreshold       int       `json:"approve_threshold,omitempty"`
	BlockThreshold         int       `json:"block_threshold,omitempty"`
	SourceETag             string    `json:"-"`
	SourceLastFetch        time.Time `json:"-"`
	SourceLastFetchFailure string    `json:"-"`
	// Empty means "central reports no policy is set." Distinct from
	// "fetch failed" — the latter would have SourceLastFetchFailure.
	Empty bool `json:"-"`
}

// Options configures a Fetcher.
type Options struct {
	URL          string        // central base URL, e.g. https://central.acme.internal
	Token        string        // bearer token reused from the agent's central.token
	CachePath    string        // where to persist the last successful fetch
	PollInterval time.Duration // background poll cadence; defaults to 5 min
	Logger       *log.Logger
	Client       *http.Client
}

// Fetcher owns one polling loop and the in-memory snapshot.
type Fetcher struct {
	opts Options
	mu   sync.RWMutex
	cur  *Effective
}

// New constructs a Fetcher. It does not perform any I/O; call Refresh
// first (typically with a short timeout) before reading Current.
func New(opts Options) (*Fetcher, error) {
	if opts.URL == "" {
		return nil, errors.New("centralpolicy: URL is required")
	}
	if opts.Token == "" {
		return nil, errors.New("centralpolicy: Token is required")
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Fetcher{opts: opts}, nil
}

// Refresh attempts one fetch. On success, the in-memory snapshot is
// updated and persisted to CachePath. On failure, the cache is loaded
// from disk (if any); if there's no cache either, an empty Effective is
// installed so Current() never returns nil.
func (f *Fetcher) Refresh(ctx context.Context) *Effective {
	eff, err := f.fetch(ctx)
	if err == nil {
		f.set(eff)
		if f.opts.CachePath != "" {
			if werr := writeCache(f.opts.CachePath, eff); werr != nil {
				f.opts.Logger.Printf("centralpolicy: cache write: %v", werr)
			}
		}
		return eff
	}
	f.opts.Logger.Printf("centralpolicy: fetch failed (%v); trying disk cache", err)
	if f.opts.CachePath != "" {
		if cached, cerr := readCache(f.opts.CachePath); cerr == nil {
			cached.SourceLastFetchFailure = err.Error()
			f.set(cached)
			return cached
		}
	}
	// No fetch, no cache → install empty so the engine still runs.
	empty := &Effective{
		Empty:                  true,
		SourceLastFetch:        time.Now(),
		SourceLastFetchFailure: err.Error(),
	}
	f.set(empty)
	return empty
}

// Run polls in the background. Cancel ctx to stop.
func (f *Fetcher) Run(ctx context.Context) {
	ticker := time.NewTicker(f.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.Refresh(ctx)
		}
	}
}

// Current returns the most recent snapshot. Never nil after Refresh.
func (f *Fetcher) Current() *Effective {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.cur
}

func (f *Fetcher) set(e *Effective) {
	f.mu.Lock()
	f.cur = e
	f.mu.Unlock()
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func (f *Fetcher) fetch(ctx context.Context) (*Effective, error) {
	url := strings.TrimRight(f.opts.URL, "/") + "/agent/v1/policy"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.opts.Token)
	// Send If-None-Match so the server can 304 on no-change.
	if cur := f.Current(); cur != nil && cur.SourceETag != "" {
		req.Header.Set("If-None-Match", cur.SourceETag)
	}
	resp, err := f.opts.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact central: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		// Re-use the current snapshot but stamp it as freshly verified.
		cur := f.Current()
		if cur == nil {
			return &Effective{Empty: true, SourceLastFetch: time.Now()}, nil
		}
		fresh := *cur
		fresh.SourceLastFetch = time.Now()
		fresh.SourceLastFetchFailure = ""
		return &fresh, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	var envelope struct {
		ID        int64           `json:"id"`
		ETag      string          `json:"etag"`
		Created   int64           `json:"created"`
		CreatedBy string          `json:"created_by"`
		Body      json.RawMessage `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	var parsed wirePolicy
	if len(envelope.Body) > 0 && string(envelope.Body) != "null" {
		if err := json.Unmarshal(envelope.Body, &parsed); err != nil {
			return nil, fmt.Errorf("parse policy body: %w", err)
		}
	}
	eff := &Effective{
		DenyPaths:        dedup(parsed.DenyPaths),
		ApproveThreshold: parsed.Scoring.ApproveThreshold,
		BlockThreshold:   parsed.Scoring.BlockThreshold,
		SourceETag:       envelope.ETag,
		SourceLastFetch:  time.Now(),
		Empty:            envelope.ID == 0 && envelope.ETag == "",
	}
	return eff, nil
}

// ---------------------------------------------------------------------------
// Merge with local policy
// ---------------------------------------------------------------------------

// Merge applies central policy on top of local, taking the stricter
// result. Returns the effective values to pass to policy.NewEngine.
//
//	DenyPaths        union (deduplicated, order: local first then central)
//	ApproveThreshold min of non-zero values (stricter wins)
//	BlockThreshold   min of non-zero values (stricter wins)
//
// central may be nil — in that case the local values are returned
// unchanged.
func Merge(central *Effective, localDenyPaths []string, localApproveThreshold, localBlockThreshold int) (denyPaths []string, approveThreshold, blockThreshold int) {
	if central == nil {
		return append([]string(nil), localDenyPaths...), localApproveThreshold, localBlockThreshold
	}
	seen := map[string]bool{}
	for _, p := range localDenyPaths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		denyPaths = append(denyPaths, p)
	}
	for _, p := range central.DenyPaths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		denyPaths = append(denyPaths, p)
	}
	approveThreshold = minPositive(localApproveThreshold, central.ApproveThreshold)
	blockThreshold = minPositive(localBlockThreshold, central.BlockThreshold)
	return
}

func minPositive(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func dedup(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// ---------------------------------------------------------------------------
// Wire shape — matches the JSON an admin pastes into the dashboard editor.
// ---------------------------------------------------------------------------

type wirePolicy struct {
	DenyPaths []string `json:"deny_paths,omitempty"`
	Scoring   struct {
		ApproveThreshold int `json:"approve_threshold,omitempty"`
		BlockThreshold   int `json:"block_threshold,omitempty"`
	} `json:"scoring,omitempty"`
}

// ---------------------------------------------------------------------------
// Disk cache
// ---------------------------------------------------------------------------

type cachedSnapshot struct {
	ETag      string    `json:"etag"`
	FetchedAt time.Time `json:"fetched_at"`
	Body      Effective `json:"body"`
}

func writeCache(path string, e *Effective) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	snap := cachedSnapshot{
		ETag:      e.SourceETag,
		FetchedAt: e.SourceLastFetch,
		Body:      *e,
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cache.tmp.")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func readCache(path string) (*Effective, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap cachedSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	e := snap.Body
	e.SourceETag = snap.ETag
	e.SourceLastFetch = snap.FetchedAt
	return &e, nil
}
