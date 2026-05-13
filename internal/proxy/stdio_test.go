package proxy

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestFrameReader_NDJSON(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
	}, "\n") + "\n"

	fr := NewFrameReader(strings.NewReader(input))
	var seen []string
	for {
		_, m, err := fr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		seen = append(seen, string(m.Classify()))
	}
	want := []string{"request", "request", "response"}
	if len(seen) != len(want) {
		t.Fatalf("count mismatch: %v vs %v", seen, want)
	}
	for i, s := range seen {
		if s != want[i] {
			t.Errorf("at %d: want %q got %q", i, want[i], s)
		}
	}
}

func TestFrameReader_PrettyPrintedJSON(t *testing.T) {
	// Some non-spec servers pretty-print across multiple lines. json.Decoder
	// handles whitespace correctly so this should still parse as one message.
	input := `{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "ping"
}`
	fr := NewFrameReader(strings.NewReader(input))
	_, m, err := fr.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if m.Method != "ping" {
		t.Errorf("method: %q", m.Method)
	}
}

func TestFrameWriter_AppendsNewline(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	m := &Message{JSONRPC: "2.0", Method: "x"}
	if err := fw.Write(m); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte{'\n'}) {
		t.Errorf("frame writer must end with newline, got %q", buf.Bytes())
	}
}

func TestFrameWriter_Concurrent(t *testing.T) {
	// FrameWriter must serialize writes so a goroutine cannot interleave
	// bytes mid-message with another.
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	done := make(chan struct{})
	const n = 50
	for i := 0; i < n; i++ {
		go func(i int) {
			_ = fw.Write(&Message{JSONRPC: "2.0", Method: "x"})
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	// Each message must be complete JSON on its own line.
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != n {
		t.Errorf("line count: want %d got %d", n, len(lines))
	}
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "{") || !strings.HasSuffix(ln, "}") {
			t.Errorf("line not complete JSON object: %q", ln)
		}
	}
}
