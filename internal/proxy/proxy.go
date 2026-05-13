package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ironmark056/sentinel/internal/approval"
	"github.com/ironmark056/sentinel/internal/audit"
	"github.com/ironmark056/sentinel/internal/policy"
)

// Upstream describes a single MCP server to launch and proxy.
//
// Exactly one of (Command, URL) should be set:
//   - Command + Args + Env → stdio subprocess (slice 1)
//   - URL + optional Headers → Streamable HTTP remote server (slice 6)
type Upstream struct {
	Name    string

	// stdio transport
	Command string
	Args    []string
	// Env is the exact environment slice (KEY=VALUE) passed to the upstream.
	// Construct with config.FilterEnv to get the security-policy-aware form.
	// If nil, the proxy inherits its own environment (slice 1 behavior).
	Env []string

	// HTTP transport
	URL     string
	Headers map[string]string
}

// Options configures a Proxy.
type Options struct {
	Upstream Upstream
	Audit    *audit.Log

	// Policy is the security engine. nil means "no policy" — every call
	// is forwarded. Used by tests that want bare passthrough.
	Policy *policy.Engine

	// Approvals is where DecisionApprove tool calls land while waiting for
	// human resolution. Required if Policy can return DecisionApprove.
	// Pass nil to treat Approve as Block.
	Approvals *approval.Store

	// ApprovalTimeout is how long the proxy will wait for a human decision
	// before defaulting to deny. Zero means use 60 seconds.
	ApprovalTimeout time.Duration

	// ClientIn / ClientOut are the streams facing the AI client. Typically
	// os.Stdin and os.Stdout when the proxy is itself launched by the client.
	ClientIn  io.Reader
	ClientOut io.Writer

	// Logger is where diagnostic messages go. nil means log to stderr.
	Logger *log.Logger
}

// Proxy is one running stdio proxy session.
type Proxy struct {
	opts            Options
	sessionID       string
	startedAt       time.Time
	upstream        upstreamConn
	clientRd        *FrameReader
	clientWr        *FrameWriter
	logger          *log.Logger
	lastBlockReason string // set during evaluate, read by sendPolicyBlock
}

// New constructs a Proxy. Run is what actually starts it.
func New(opts Options) (*Proxy, error) {
	if opts.ClientIn == nil {
		opts.ClientIn = os.Stdin
	}
	if opts.ClientOut == nil {
		opts.ClientOut = os.Stdout
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "[sentinel] ", log.LstdFlags|log.Lmicroseconds)
	}
	return &Proxy{
		opts:      opts,
		sessionID: uuid.NewString(),
		startedAt: time.Now(),
		logger:    opts.Logger,
	}, nil
}

// SessionID returns this run's session identifier.
func (p *Proxy) SessionID() string { return p.sessionID }

// Run brings the upstream online, wires up the streams, and proxies
// messages in both directions until either side closes or ctx is cancelled.
// Run is blocking.
func (p *Proxy) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, err := dialUpstream(ctx, p.opts.Upstream, p.logger)
	if err != nil {
		return err
	}
	p.upstream = conn
	defer p.upstream.Close()

	p.clientRd = NewFrameReader(p.opts.ClientIn)
	p.clientWr = NewFrameWriter(p.opts.ClientOut)

	p.logger.Printf("session %s started: upstream=%s",
		p.sessionID, p.upstream.Description())

	var wg sync.WaitGroup
	wg.Add(2)
	errCh := make(chan error, 2)

	go func() {
		defer wg.Done()
		err := p.pumpClientToUpstream(ctx)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			p.logger.Printf("c2s pump exited: %v", err)
		}
		errCh <- err
		cancel()
	}()

	go func() {
		defer wg.Done()
		err := p.pumpUpstreamToClient(ctx)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			p.logger.Printf("s2c pump exited: %v", err)
		}
		errCh <- err
		cancel()
	}()

	go p.drainUnparsedLines("client", p.clientRd.UnparsedLines())

	wg.Wait()
	close(errCh)

	var firstErr error
	for err := range errCh {
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && firstErr == nil {
			firstErr = err
		}
	}
	p.logger.Printf("session %s ended", p.sessionID)
	return firstErr
}

// dialUpstream picks the right transport based on Upstream config.
func dialUpstream(ctx context.Context, u Upstream, logger *log.Logger) (upstreamConn, error) {
	switch {
	case u.URL != "" && u.Command != "":
		return nil, fmt.Errorf("upstream %q: cannot set both command and url", u.Name)
	case u.URL != "":
		return dialHTTPUpstream(HTTPUpstreamOptions{
			Name:    u.Name,
			URL:     u.URL,
			Headers: u.Headers,
			Logger:  logger,
		})
	case u.Command != "":
		return spawnStdioUpstream(ctx, u, logger)
	default:
		return nil, fmt.Errorf("upstream %q: neither command nor url set", u.Name)
	}
}

func (p *Proxy) pumpClientToUpstream(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		raw, msg, err := p.clientRd.Read()
		if err != nil {
			return err
		}
		p.logMessage(raw, msg, audit.ClientToServer)

		// Run the policy engine on tool calls before forwarding.
		decision := p.evaluate(ctx, msg, raw)
		switch decision {
		case verdictAllow:
			if err := p.upstream.Send(raw); err != nil {
				return fmt.Errorf("forward to upstream: %w", err)
			}
		case verdictBlock:
			if err := p.sendPolicyBlock(msg); err != nil {
				return fmt.Errorf("write block response: %w", err)
			}
		}
	}
}

// verdict is the proxy's collapsed decision after running the policy engine
// (and the approval wait, if applicable).
type verdict int

const (
	verdictAllow verdict = iota
	verdictBlock
)

// evaluate inspects msg, runs the policy engine, and — if needed — performs
// the approval wait. Returns the final verdict.
func (p *Proxy) evaluate(ctx context.Context, msg *Message, raw []byte) verdict {
	if p.opts.Policy == nil {
		return verdictAllow
	}
	if msg.Method != "tools/call" {
		return verdictAllow
	}
	toolName, args := extractToolCall(msg.Params)
	res := p.opts.Policy.Evaluate(msg.Method, toolName, args)

	// Log every finding for visibility, regardless of decision.
	for _, f := range res.Findings {
		p.logger.Printf("FINDING(%s) %s/%s at %s",
			f.Severity, f.Category, f.Rule, f.JSONPath)
	}

	// Consult auto-decisions before honoring the engine's verdict.
	// Auto-deny is a hard override: if ANY finding's rule has an auto-deny,
	// the call is blocked even if the engine would have allowed it.
	// Auto-allow only short-circuits APPROVE-tier decisions: it never
	// overrides a Critical/stacked-finding Block. This is the safety
	// property — see docs/08-approval-flow.md.
	if override, applied := p.applyAutoDecisions(ctx, &res, toolName, msg); applied {
		return override
	}

	switch res.Decision {
	case policy.DecisionAllow:
		return verdictAllow
	case policy.DecisionBlock:
		p.lastBlockReason = describeFinding(res.BlockReason, res.RiskScore)
		p.logger.Printf("BLOCKED %s id=%s tool=%s score=%d: %s",
			msg.Method, msg.IDString(), toolName, res.RiskScore, p.lastBlockReason)
		return verdictBlock
	case policy.DecisionApprove:
		return p.handleApproval(ctx, msg, raw, toolName, &res)
	}
	return verdictAllow
}

// applyAutoDecisions consults the persisted rule-level auto-decisions and
// returns an override verdict when applicable.
//
//	1) If any finding's rule is on the auto-deny list → verdictBlock.
//	2) If the engine's decision is Approve AND every finding's rule has
//	   an auto-allow → verdictAllow.
//	3) Otherwise → no override (caller proceeds with engine.Decision).
//
// The asymmetry is deliberate: auto-allow only short-circuits the human
// prompt; it never overrides a hard Block from the engine.
func (p *Proxy) applyAutoDecisions(ctx context.Context, res *policy.Result, toolName string, msg *Message) (verdict, bool) {
	if p.opts.Approvals == nil || len(res.Findings) == 0 {
		return 0, false
	}
	autoAllowed := 0
	for _, f := range res.Findings {
		ruleID := string(f.Category) + "/" + f.Rule
		dec, ok, err := p.opts.Approvals.GetAutoDecision(ctx, ruleID)
		if err != nil {
			p.logger.Printf("auto-decision lookup failed for %s: %v (ignoring)", ruleID, err)
			continue
		}
		if !ok {
			continue
		}
		switch dec {
		case approval.StatusDenied:
			p.lastBlockReason = fmt.Sprintf("auto-denied by rule %q", ruleID)
			p.logger.Printf("AUTO-DENY %s id=%s tool=%s rule=%s",
				msg.Method, msg.IDString(), toolName, ruleID)
			return verdictBlock, true
		case approval.StatusApproved:
			autoAllowed++
		}
	}
	// Auto-allow only kicks in for Approve-tier decisions, and only if
	// every finding's rule is covered.
	if res.Decision == policy.DecisionApprove && autoAllowed == len(res.Findings) {
		p.logger.Printf("AUTO-ALLOW %s id=%s tool=%s (all %d findings covered by auto-allow rules)",
			msg.Method, msg.IDString(), toolName, autoAllowed)
		return verdictAllow, true
	}
	return 0, false
}

// handleApproval inserts a pending row and blocks until resolved or timeout.
func (p *Proxy) handleApproval(ctx context.Context, msg *Message, raw []byte, toolName string, res *policy.Result) verdict {
	if p.opts.Approvals == nil {
		// No approval store wired in → default-deny rather than silently
		// allowing things into the "approve" range.
		p.lastBlockReason = fmt.Sprintf("score=%d requires approval but no approval store configured; defaulting to deny", res.RiskScore)
		p.logger.Printf("APPROVAL-MISCONFIGURED %s id=%s tool=%s: %s",
			msg.Method, msg.IDString(), toolName, p.lastBlockReason)
		return verdictBlock
	}

	findingsJSON, _ := json.Marshal(res.Findings)
	id, err := p.opts.Approvals.Insert(ctx, approval.Approval{
		CreatedAt:    time.Now(),
		SessionID:    p.sessionID,
		Upstream:     p.opts.Upstream.Name,
		MsgID:        msg.IDString(),
		Method:       msg.Method,
		ToolName:     toolName,
		RiskScore:    res.RiskScore,
		FindingsJSON: findingsJSON,
		Payload:      raw,
	})
	if err != nil {
		p.lastBlockReason = "approval store write failed; defaulting to deny"
		p.logger.Printf("APPROVAL-INSERT-FAILED id=%s tool=%s: %v", msg.IDString(), toolName, err)
		return verdictBlock
	}

	p.logger.Printf("APPROVAL-PENDING id=%d msg_id=%s tool=%s score=%d — waiting for human...",
		id, msg.IDString(), toolName, res.RiskScore)

	timeout := p.opts.ApprovalTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	status, err := p.opts.Approvals.WaitForDecision(waitCtx, id, 500*time.Millisecond)
	if err != nil {
		p.lastBlockReason = "approval wait failed; defaulting to deny"
		p.logger.Printf("APPROVAL-WAIT-FAILED id=%d: %v", id, err)
		return verdictBlock
	}

	switch status {
	case approval.StatusApproved:
		p.logger.Printf("APPROVAL-APPROVED id=%d tool=%s — forwarding", id, toolName)
		return verdictAllow
	case approval.StatusDenied:
		p.lastBlockReason = fmt.Sprintf("denied by user (approval #%d)", id)
		p.logger.Printf("APPROVAL-DENIED id=%d tool=%s", id, toolName)
		return verdictBlock
	case approval.StatusTimeout:
		_ = p.opts.Approvals.Resolve(context.Background(), id, approval.StatusTimeout, "(timeout)")
		p.lastBlockReason = fmt.Sprintf("approval #%d timed out after %s (default deny)", id, timeout)
		p.logger.Printf("APPROVAL-TIMEOUT id=%d tool=%s", id, toolName)
		return verdictBlock
	}
	p.lastBlockReason = fmt.Sprintf("unexpected approval status %q (treating as deny)", status)
	return verdictBlock
}

func describeFinding(f *policy.Finding, score int) string {
	if f == nil {
		return fmt.Sprintf("policy block (score=%d)", score)
	}
	return fmt.Sprintf("[%s/%s] %s (score=%d)",
		f.Category, f.Rule, f.Description, score)
}

// sendPolicyBlock writes a JSON-RPC error response back to the client for
// the blocked request, so the client knows the call did not reach upstream.
func (p *Proxy) sendPolicyBlock(req *Message) error {
	resp := &Message{
		JSONRPC: "2.0",
		ID:      req.ID,
		Error: &RPCError{
			Code:    -32000, // JSON-RPC application error
			Message: "blocked by sentinel policy: " + p.lastBlockReason,
		},
	}
	if err := p.clientWr.Write(resp); err != nil {
		return err
	}
	// Also log the synthesized response so the audit shows what the client
	// actually saw.
	raw, _ := Encode(resp)
	p.logMessage(raw, resp, audit.ServerToClient)
	return nil
}

// extractToolCall pulls the inner tool name and arguments from the params
// of a tools/call request. Missing fields return zero values.
func extractToolCall(params json.RawMessage) (string, json.RawMessage) {
	if len(params) == 0 {
		return "", nil
	}
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", params
	}
	return p.Name, p.Arguments
}

func (p *Proxy) pumpUpstreamToClient(ctx context.Context) error {
	for {
		raw, msg, err := p.upstream.NextFrame(ctx)
		if err != nil {
			return err
		}
		p.logMessage(raw, msg, audit.ServerToClient)
		if err := p.clientWr.WriteRaw(raw); err != nil {
			return fmt.Errorf("forward to client: %w", err)
		}
	}
}

func (p *Proxy) logMessage(raw []byte, m *Message, dir audit.Direction) {
	if p.opts.Audit == nil {
		return
	}
	p.opts.Audit.Append(audit.Event{
		TS:        time.Now(),
		SessionID: p.sessionID,
		Upstream:  p.opts.Upstream.Name,
		Direction: dir,
		MsgType:   string(m.Classify()),
		MsgID:     m.IDString(),
		Method:    m.MethodOrEmpty(),
		Payload:   raw,
		Bytes:     len(raw),
	})
}

func (p *Proxy) drainUnparsedLines(label string, ch <-chan []byte) {
	for line := range ch {
		p.logger.Printf("%s stray line: %s", label, line)
	}
}
