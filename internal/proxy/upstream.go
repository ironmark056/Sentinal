package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
)

// upstreamConn abstracts over the two MCP wire transports the proxy
// supports: a local stdio subprocess (slice 1) and a remote Streamable-HTTP
// endpoint (slice 6). The proxy's hot path is transport-agnostic — it asks
// for the next frame, writes a frame, and shuts down.
type upstreamConn interface {
	// Send forwards the raw JSON-RPC envelope upstream.
	Send(raw []byte) error
	// NextFrame returns the next received envelope, or io.EOF when the
	// upstream stream ends.
	NextFrame(ctx context.Context) (raw []byte, msg *Message, err error)
	// Close shuts down the upstream. Idempotent.
	Close() error
	// Description is a short string for logs.
	Description() string
}

// ---------------------------------------------------------------------------
// stdio upstream — wraps a local subprocess.
// ---------------------------------------------------------------------------

type stdioUpstream struct {
	name    string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	rd      *FrameReader
	wr      *FrameWriter
	stderr  io.ReadCloser
	logger  *log.Logger
}

func spawnStdioUpstream(ctx context.Context, u Upstream, logger *log.Logger) (upstreamConn, error) {
	cmd := exec.CommandContext(ctx, u.Command, u.Args...)
	if u.Env != nil {
		cmd.Env = u.Env
	} else {
		cmd.Env = os.Environ()
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("upstream stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("upstream stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("upstream stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start upstream %q: %w", u.Command, err)
	}

	su := &stdioUpstream{
		name:   u.Name,
		cmd:    cmd,
		stdin:  stdin,
		rd:     NewFrameReader(stdout),
		wr:     NewFrameWriter(stdin),
		stderr: stderr,
		logger: logger,
	}
	go su.drainStderr()
	go su.drainUnparsed()
	return su, nil
}

func (s *stdioUpstream) Send(raw []byte) error            { return s.wr.WriteRaw(raw) }
func (s *stdioUpstream) Description() string              { return fmt.Sprintf("stdio:%s", s.name) }

func (s *stdioUpstream) NextFrame(ctx context.Context) ([]byte, *Message, error) {
	// FrameReader does not take ctx; cancellation is handled by closing the
	// underlying pipe in Close(), which surfaces as an error from Read().
	return s.rd.Read()
}

func (s *stdioUpstream) Close() error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	_ = s.stdin.Close()
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()
	s.cmd = nil
	return nil
}

func (s *stdioUpstream) drainStderr() {
	if s.stderr == nil {
		return
	}
	sc := bufio.NewScanner(s.stderr)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		s.logger.Printf("upstream(%s) stderr: %s", s.name, sc.Text())
	}
}

func (s *stdioUpstream) drainUnparsed() {
	for line := range s.rd.UnparsedLines() {
		s.logger.Printf("upstream(%s) stray line: %s", s.name, line)
	}
}
