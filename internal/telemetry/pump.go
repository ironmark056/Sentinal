// Package telemetry ships an agent's local audit events to a central
// sentinel-server. Off by default; activated when the agent's config has
// a `central:` block.
//
// Design:
//   - The agent never deletes events from its local audit log. Loss of
//     connectivity to central does not lose data; the pump simply pauses
//     and resumes from the persisted cursor when central comes back.
//   - The pump runs as a goroutine alongside the proxy session. Failures
//     are logged but never bring down the proxy hot path.
//   - All events are pushed in order by id, in batches.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ironmark056/sentinel/internal/audit"
)

// Options configures the pump.
type Options struct {
	URL       string        // central server base URL, e.g. https://central.acme.internal
	Token     string        // bearer token issued by sentinel-server agent create
	AgentName string        // display name, included in metadata
	Audit     *audit.Log    // local audit log to pull from
	Interval  time.Duration // poll cadence; default 5s
	BatchSize int           // max events per POST; default 100
	Logger    *log.Logger
	Client    *http.Client // optional; default has a 30s overall timeout
}

// Pump is one running telemetry pump.
type Pump struct {
	opts      Options
	cursor    int64
	cursorKey string
	logger    *log.Logger
	stop      chan struct{}
	wg        sync.WaitGroup
}

// New constructs a Pump but does not start it. Call Run.
func New(opts Options) (*Pump, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("telemetry: URL is required")
	}
	if opts.Token == "" {
		return nil, fmt.Errorf("telemetry: Token is required")
	}
	if opts.Audit == nil {
		return nil, fmt.Errorf("telemetry: Audit log is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Second
	}
	if opts.BatchSize <= 0 || opts.BatchSize > 1000 {
		opts.BatchSize = 100
	}
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Pump{
		opts:      opts,
		cursorKey: "central:" + canonURL(opts.URL),
		logger:    opts.Logger,
		stop:      make(chan struct{}),
	}, nil
}

// Run starts the pump and blocks until ctx is cancelled or Stop is called.
func (p *Pump) Run(ctx context.Context) error {
	// Load cursor.
	cur, err := p.opts.Audit.GetCursor(ctx, p.cursorKey)
	if err != nil {
		p.logger.Printf("telemetry: read cursor failed: %v (starting from 0)", err)
	} else if cur != "" {
		if v, err := strconv.ParseInt(cur, 10, 64); err == nil {
			p.cursor = v
		}
	}
	p.logger.Printf("telemetry: pump started → %s (agent=%s, cursor=%d)",
		p.opts.URL, p.opts.AgentName, p.cursor)

	p.wg.Add(1)
	defer p.wg.Done()

	ticker := time.NewTicker(p.opts.Interval)
	defer ticker.Stop()

	// One immediate flush on startup.
	p.flushOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			p.logger.Printf("telemetry: pump stopping (ctx done)")
			return ctx.Err()
		case <-p.stop:
			return nil
		case <-ticker.C:
			p.flushOnce(ctx)
		}
	}
}

// Stop signals the pump to exit and waits for the goroutine to finish.
func (p *Pump) Stop() {
	close(p.stop)
	p.wg.Wait()
}

// flushOnce pulls the next batch of events and POSTs them. Errors are
// logged; the cursor is only advanced on success.
func (p *Pump) flushOnce(ctx context.Context) {
	events, err := p.opts.Audit.ReadEventsAfter(ctx, p.cursor, p.opts.BatchSize)
	if err != nil {
		p.logger.Printf("telemetry: read after %d: %v", p.cursor, err)
		return
	}
	if len(events) == 0 {
		return
	}

	body := ingestBody{Events: make([]ingestEvent, 0, len(events))}
	for _, e := range events {
		body.Events = append(body.Events, ingestEvent{
			AgentTS:   e.TS.UnixNano(),
			SessionID: e.SessionID,
			Upstream:  e.Upstream,
			Direction: string(e.Direction),
			MsgType:   e.MsgType,
			MsgID:     e.MsgID,
			Method:    e.Method,
			Payload:   json.RawMessage(e.Payload),
			Bytes:     e.Bytes,
		})
	}

	if err := p.post(ctx, body); err != nil {
		p.logger.Printf("telemetry: POST failed (will retry next tick): %v", err)
		return
	}

	newCursor := events[len(events)-1].ID
	if err := p.opts.Audit.SetCursor(ctx, p.cursorKey, strconv.FormatInt(newCursor, 10)); err != nil {
		p.logger.Printf("telemetry: persist cursor failed: %v (will re-push)", err)
		return
	}
	p.cursor = newCursor
	p.logger.Printf("telemetry: shipped %d events (cursor=%d)", len(events), p.cursor)
}

func (p *Pump) post(ctx context.Context, body ingestBody) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := strings.TrimRight(p.opts.URL, "/") + "/agent/v1/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.opts.Token)

	resp, err := p.opts.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, preview)
	}
	// Consume body so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// CheckHealth verifies the central server is reachable with the configured
// token. Useful for `sentinel run` to fail fast on misconfig.
func (p *Pump) CheckHealth(ctx context.Context) error {
	url := strings.TrimRight(p.opts.URL, "/") + "/agent/v1/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.opts.Token)
	resp, err := p.opts.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("health check: HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wire types (mirror internal/server.IngestRequest/IngestEvent)
// ---------------------------------------------------------------------------

type ingestBody struct {
	Events []ingestEvent `json:"events"`
}

type ingestEvent struct {
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

// canonURL normalizes a URL for use as a state-table key (so the same
// agent talking to different central servers keeps separate cursors).
func canonURL(u string) string {
	u = strings.TrimRight(u, "/")
	u = strings.ToLower(u)
	return u
}
