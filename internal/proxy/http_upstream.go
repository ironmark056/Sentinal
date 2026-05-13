package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// httpUpstream speaks Streamable HTTP (the current MCP HTTP transport) to a
// remote MCP server. Client→server JSON-RPC envelopes are POSTed to a single
// URL. Each response is either a one-shot JSON envelope or a Server-Sent
// Events stream of envelopes.
//
// Slice 6 scope:
//   - POST for outbound messages (requests, notifications, responses)
//   - parse both application/json and text/event-stream responses
//   - track Mcp-Session-Id assigned by the server
//   - forward custom auth/admin headers from config
//
// Out of scope for this slice (folded into a future slice 6.x):
//   - GET-based server-initiated streams (long-lived SSE for unsolicited
//     server-to-client messages — rare in MCP today)
//   - Last-Event-Id resumability
//   - automatic reconnect on transport errors
type httpUpstream struct {
	name      string
	url       string
	headers   map[string]string
	client    *http.Client
	logger    *log.Logger

	sessionMu sync.Mutex
	sessionID string

	recv      chan []byte // received raw envelopes from response bodies
	wg        sync.WaitGroup

	closeOnce sync.Once
	closed    chan struct{}
}

// HTTPUpstreamOptions configures a remote upstream.
type HTTPUpstreamOptions struct {
	Name    string
	URL     string
	Headers map[string]string // optional auth / org headers
	Logger  *log.Logger
	Client  *http.Client // optional; defaults to a sensible client
}

// dialHTTPUpstream returns an upstreamConn that speaks Streamable HTTP.
// The conn does not perform any I/O until the proxy calls Send.
func dialHTTPUpstream(opts HTTPUpstreamOptions) (upstreamConn, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("http upstream: url is required")
	}
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	cl := opts.Client
	if cl == nil {
		cl = &http.Client{
			Timeout: 0, // no timeout — SSE streams are long-lived
			Transport: &http.Transport{
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		}
	}
	return &httpUpstream{
		name:    opts.Name,
		url:     opts.URL,
		headers: opts.Headers,
		client:  cl,
		logger:  opts.Logger,
		recv:    make(chan []byte, 64),
		closed:  make(chan struct{}),
	}, nil
}

func (h *httpUpstream) Description() string {
	return fmt.Sprintf("http:%s", h.name)
}

// Send POSTs the raw envelope and starts a goroutine to drain whatever the
// response is (one-shot JSON or SSE stream). Returns once the POST has been
// dispatched, not when the response is fully consumed.
func (h *httpUpstream) Send(raw []byte) error {
	select {
	case <-h.closed:
		return errors.New("upstream closed")
	default:
	}

	h.wg.Add(1)
	go h.postAndStream(raw)
	return nil
}

func (h *httpUpstream) postAndStream(raw []byte) {
	defer h.wg.Done()

	req, err := http.NewRequest(http.MethodPost, h.url, bytes.NewReader(raw))
	if err != nil {
		h.logger.Printf("upstream(%s) build request: %v", h.name, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	h.sessionMu.Lock()
	if h.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", h.sessionID)
	}
	h.sessionMu.Unlock()

	resp, err := h.client.Do(req)
	if err != nil {
		h.logger.Printf("upstream(%s) POST failed: %v", h.name, err)
		return
	}
	defer resp.Body.Close()

	// Capture session id if the server is assigning one.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.sessionMu.Lock()
		if h.sessionID != sid {
			h.sessionID = sid
			h.logger.Printf("upstream(%s) session id: %s", h.name, sid)
		}
		h.sessionMu.Unlock()
	}

	if resp.StatusCode == http.StatusAccepted {
		// 202 with no body: notification ack, no response expected.
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		h.logger.Printf("upstream(%s) HTTP %d: %s", h.name, resp.StatusCode, body)
		return
	}

	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		h.consumeSSE(resp.Body)
	case strings.HasPrefix(ct, "application/json"):
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			h.logger.Printf("upstream(%s) read body: %v", h.name, err)
			return
		}
		h.deliver(body)
	default:
		h.logger.Printf("upstream(%s) unexpected content-type %q", h.name, ct)
	}
}

// consumeSSE reads the response as a Server-Sent Events stream and pushes
// each event's data payload onto the recv channel.
//
// SSE wire format we care about (the rest is ignored):
//
//	data: {"jsonrpc":"2.0", ...}
//	(blank line ends the event)
//
// Multi-line data fields are joined with "\n" per the SSE spec. We do not
// honor `event:` types — MCP only uses the default "message" event.
func (h *httpUpstream) consumeSSE(body io.Reader) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4<<20)

	var dataBuf bytes.Buffer
	flush := func() {
		if dataBuf.Len() == 0 {
			return
		}
		// Copy out of the shared buffer.
		payload := make([]byte, dataBuf.Len())
		copy(payload, dataBuf.Bytes())
		dataBuf.Reset()
		h.deliver(payload)
	}

	for sc.Scan() {
		line := sc.Text()
		// Empty line → dispatch event.
		if line == "" {
			flush()
			continue
		}
		// Comment line per SSE spec: starts with ':'.
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(data)
			continue
		}
		// id:, event:, retry: — we don't use them for MCP.
	}
	flush() // unterminated event at stream end
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		h.logger.Printf("upstream(%s) SSE scan error: %v", h.name, err)
	}
}

func (h *httpUpstream) deliver(raw []byte) {
	select {
	case h.recv <- raw:
	case <-h.closed:
	}
}

// NextFrame returns the next received envelope. Blocks until one is
// available or the upstream is closed.
func (h *httpUpstream) NextFrame(ctx context.Context) ([]byte, *Message, error) {
	select {
	case raw := <-h.recv:
		msg, err := Decode(raw)
		if err != nil {
			h.logger.Printf("upstream(%s) decode failed: %v (payload: %s)", h.name, err, truncate(raw, 200))
			// Skip and try the next frame.
			return h.NextFrame(ctx)
		}
		return raw, msg, nil
	case <-h.closed:
		return nil, nil, io.EOF
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (h *httpUpstream) Close() error {
	h.closeOnce.Do(func() {
		close(h.closed)
	})
	// Wait briefly for in-flight POSTs to finish so their goroutines exit.
	// They will see h.closed via the recv send select and bail.
	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		// Outstanding HTTP requests will eventually finish or fail; we don't
		// block shutdown longer than this.
	}
	return nil
}

// truncate is a small helper for log readability.
func truncate(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	out := make([]byte, max+3)
	copy(out, b[:max])
	copy(out[max:], "...")
	return out
}
