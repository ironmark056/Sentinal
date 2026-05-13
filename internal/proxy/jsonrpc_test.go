package proxy

import (
	"encoding/json"
	"testing"
)

func TestClassify_Request(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`)
	m, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := m.Classify(); got != TypeRequest {
		t.Errorf("want %q got %q", TypeRequest, got)
	}
	if m.IDString() != "1" {
		t.Errorf("want id=1, got %q", m.IDString())
	}
	if m.Method != "tools/call" {
		t.Errorf("method mismatch: %q", m.Method)
	}
}

func TestClassify_Notification(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	m, _ := Decode(raw)
	if got := m.Classify(); got != TypeNotification {
		t.Errorf("want notification, got %q", got)
	}
	if m.IDString() != "" {
		t.Errorf("expected empty id, got %q", m.IDString())
	}
}

func TestClassify_Response(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":42,"result":{"ok":true}}`)
	m, _ := Decode(raw)
	if got := m.Classify(); got != TypeResponse {
		t.Errorf("want response, got %q", got)
	}
	if m.IDString() != "42" {
		t.Errorf("want id=42, got %q", m.IDString())
	}
}

func TestClassify_Error(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":42,"error":{"code":-32000,"message":"boom"}}`)
	m, _ := Decode(raw)
	if got := m.Classify(); got != TypeError {
		t.Errorf("want error, got %q", got)
	}
	if m.Error == nil || m.Error.Code != -32000 {
		t.Errorf("error decode: %+v", m.Error)
	}
}

func TestClassify_StringID(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":"abc-123","method":"ping"}`)
	m, _ := Decode(raw)
	if got := m.Classify(); got != TypeRequest {
		t.Errorf("want request, got %q", got)
	}
	if m.IDString() != "abc-123" {
		t.Errorf("want id=abc-123, got %q", m.IDString())
	}
}

func TestClassify_NullID(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":null,"method":"x"}`)
	m, _ := Decode(raw)
	// Per JSON-RPC spec, id:null can appear in error responses to invalid
	// requests. We treat null id like missing id, so this becomes a
	// notification.
	if got := m.Classify(); got != TypeNotification {
		t.Errorf("want notification (null id), got %q", got)
	}
}

func TestClassify_MissingJSONRPCField(t *testing.T) {
	// Some servers in the wild omit "jsonrpc": "2.0". We tolerate this.
	raw := []byte(`{"id":1,"method":"tools/list"}`)
	m, _ := Decode(raw)
	if got := m.Classify(); got != TypeRequest {
		t.Errorf("want request, got %q", got)
	}
}

func TestEncodeRoundtrip(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":7,"method":"x","params":{"a":1}}`)
	m, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	// Re-parse to verify semantic equivalence
	m2, err := Decode(out)
	if err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if m2.Method != m.Method || m2.IDString() != m.IDString() {
		t.Errorf("roundtrip differs: %+v vs %+v", m, m2)
	}
}

func TestDecode_BadJSON(t *testing.T) {
	_, err := Decode([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected error on bad JSON")
	}
}

func TestDecode_RawMessageParamsPreserved(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"x","params":{"nested":{"deep":[1,2,3]}}}`)
	m, _ := Decode(raw)
	var got map[string]any
	if err := json.Unmarshal(m.Params, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["nested"]; !ok {
		t.Errorf("nested params lost: %+v", got)
	}
}
