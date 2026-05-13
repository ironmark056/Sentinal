// echomcp is a minimal MCP-shaped upstream used in tests and smoke checks.
// It reads newline-delimited JSON-RPC from stdin and replies to any request
// with an "echo" result. Notifications are silently consumed.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type envelope struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var e envelope
		if err := json.Unmarshal(line, &e); err != nil {
			fmt.Fprintf(os.Stderr, "echomcp: skip non-json line: %v\n", err)
			continue
		}
		if e.Method == "" {
			// Probably a response coming back; we don't do those.
			continue
		}
		if len(e.ID) == 0 || string(e.ID) == "null" {
			// Notification — no reply expected
			continue
		}
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(e.ID),
			"result": map[string]any{
				"echo":   e.Method,
				"params": json.RawMessage(e.Params),
			},
		}
		out, _ := json.Marshal(resp)
		fmt.Println(string(out))
	}
}
