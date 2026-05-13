// Package proxy handles the MCP JSON-RPC wire protocol and message dispatch.
package proxy

import (
	"encoding/json"
	"fmt"
)

// Message is a single JSON-RPC 2.0 envelope. It is permissive on purpose —
// in the wild we see servers that omit "jsonrpc", send "id" as a string, or
// pretty-print across multiple lines. We accept these shapes and only fail
// when the envelope cannot be interpreted at all.
type Message struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// MessageType classifies an envelope after parsing.
type MessageType string

const (
	TypeRequest      MessageType = "request"
	TypeResponse     MessageType = "response"
	TypeError        MessageType = "error"
	TypeNotification MessageType = "notification"
	TypeUnknown      MessageType = "unknown"
)

// Classify returns the MessageType implied by the envelope's populated fields.
//
//	request:      has method, has id
//	notification: has method, no id
//	response:     no method, has id, has result
//	error:        no method, has id, has error
func (m *Message) Classify() MessageType {
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	hasMethod := m.Method != ""
	hasResult := len(m.Result) > 0
	hasError := m.Error != nil

	switch {
	case hasMethod && hasID:
		return TypeRequest
	case hasMethod && !hasID:
		return TypeNotification
	case !hasMethod && hasID && hasError:
		return TypeError
	case !hasMethod && hasID && hasResult:
		return TypeResponse
	case !hasMethod && hasID:
		// Response with empty result is still a response.
		return TypeResponse
	default:
		return TypeUnknown
	}
}

// IDString returns the envelope's id as a string for indexing purposes.
// Numeric ids are formatted as their JSON representation ("42"), string ids
// retain their quotes-stripped value, null returns "".
func (m *Message) IDString() string {
	if len(m.ID) == 0 {
		return ""
	}
	// Try to unmarshal as string first.
	var s string
	if err := json.Unmarshal(m.ID, &s); err == nil {
		return s
	}
	// Otherwise return the raw JSON form (numbers, etc.).
	return string(m.ID)
}

// MethodOrEmpty returns the method, or an empty string for responses/errors.
func (m *Message) MethodOrEmpty() string {
	return m.Method
}

// Decode parses a single JSON-RPC envelope from raw bytes. Returns the
// envelope plus the raw bytes (caller may want the original for logging).
func Decode(raw []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode jsonrpc: %w", err)
	}
	return &m, nil
}

// Encode serializes an envelope back to JSON. Trailing newline NOT included.
func Encode(m *Message) ([]byte, error) {
	return json.Marshal(m)
}
