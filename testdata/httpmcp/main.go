// httpmcp is a minimal Streamable-HTTP-shaped upstream used in tests.
//
// It listens on the address given by --addr (default :8801) and answers
// every POST with either:
//   - a one-shot JSON response (default), or
//   - an SSE stream of two messages (when ?mode=sse is on the URL).
//
// The body is parsed as a JSON-RPC envelope; the response echoes the
// method back as the result, mirroring testdata/echomcp.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type envelope struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func main() {
	addr := flag.String("addr", ":8801", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handle)
	log.Printf("httpmcp listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var e envelope
	if err := json.Unmarshal(body, &e); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Mcp-Session-Id", "test-session-1")

	// Notifications (no id) get a 202.
	if len(e.ID) == 0 || string(e.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(e.ID),
		"result":  map[string]any{"echo": e.Method, "transport": "http"},
	}
	out, _ := json.Marshal(resp)

	if strings.Contains(r.URL.RawQuery, "mode=sse") {
		// Emit two SSE events to exercise the SSE parser.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		fmt.Fprintf(w, "data: %s\n\n", out)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(50 * time.Millisecond)

		second := map[string]any{
			"jsonrpc": "2.0",
			"method":  "notifications/progress",
			"params":  map[string]any{"progressToken": e.ID, "value": 100},
		}
		sb, _ := json.Marshal(second)
		fmt.Fprintf(w, "data: %s\n\n", sb)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	bw := bufio.NewWriter(w)
	bw.Write(out)
	bw.Flush()
}
