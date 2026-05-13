package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// FrameReader reads newline-delimited JSON-RPC envelopes from a stream.
// MCP stdio framing is one JSON message per line. We tolerate pretty-printed
// JSON via json.Decoder (which accumulates until a complete value is parsed),
// and we tolerate stray non-JSON lines by surfacing them via UnparsedLines.
type FrameReader struct {
	dec           *json.Decoder
	raw           *bufio.Reader
	unparsedLines chan []byte
	closeOnce     sync.Once
}

// NewFrameReader wraps r. Non-JSON lines are written to a channel returned
// by UnparsedLines so the caller can log them as stderr-like diagnostic
// output.
func NewFrameReader(r io.Reader) *FrameReader {
	br := bufio.NewReaderSize(r, 1<<20) // 1 MiB buffer
	return &FrameReader{
		dec:           json.NewDecoder(br),
		raw:           br,
		unparsedLines: make(chan []byte, 32),
	}
}

// UnparsedLines returns a channel of bytes that looked like text but did not
// parse as JSON. The channel is closed when Read encounters io.EOF.
func (fr *FrameReader) UnparsedLines() <-chan []byte {
	return fr.unparsedLines
}

// Read returns the next complete JSON value as raw bytes plus the parsed
// envelope. Returns io.EOF when the stream ends.
func (fr *FrameReader) Read() ([]byte, *Message, error) {
	var raw json.RawMessage
	if err := fr.dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			fr.closeOnce.Do(func() { close(fr.unparsedLines) })
			return nil, nil, io.EOF
		}
		// Decoder is now in a sync-broken state if this was a JSON syntax
		// error. Try to recover by advancing past the next newline.
		if synErr := (&json.SyntaxError{}); errors.As(err, &synErr) {
			if recovered, recoverErr := fr.recoverPastNewline(); recoverErr == nil && recovered != nil {
				select {
				case fr.unparsedLines <- recovered:
				default:
					// channel full, drop
				}
				return fr.Read()
			}
		}
		return nil, nil, fmt.Errorf("read frame: %w", err)
	}

	msg, err := Decode(raw)
	if err != nil {
		// Looked like JSON, parsed as JSON, but not as a valid envelope.
		// Surface as unparsed and continue.
		select {
		case fr.unparsedLines <- []byte(raw):
		default:
		}
		return fr.Read()
	}
	return []byte(raw), msg, nil
}

// recoverPastNewline reads bytes from the underlying buffer until a newline,
// returning what was consumed. The json.Decoder is then rebuilt against the
// buffer's remaining content.
func (fr *FrameReader) recoverPastNewline() ([]byte, error) {
	line, err := fr.raw.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// Rebuild decoder against the remainder.
	fr.dec = json.NewDecoder(fr.raw)
	return line, nil
}

// FrameWriter writes JSON-RPC envelopes to a stream as newline-delimited JSON.
// Writes are serialized internally so multiple goroutines can call Write
// concurrently without interleaving bytes mid-message.
type FrameWriter struct {
	w  io.Writer
	mu sync.Mutex
}

func NewFrameWriter(w io.Writer) *FrameWriter {
	return &FrameWriter{w: w}
}

// Write encodes m and emits it as one line (NDJSON).
func (fw *FrameWriter) Write(m *Message) error {
	b, err := Encode(m)
	if err != nil {
		return err
	}
	return fw.WriteRaw(b)
}

// WriteRaw emits already-encoded JSON bytes followed by a newline.
func (fw *FrameWriter) WriteRaw(b []byte) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if _, err := fw.w.Write(b); err != nil {
		return err
	}
	_, err := fw.w.Write([]byte{'\n'})
	return err
}
