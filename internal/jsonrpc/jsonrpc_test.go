package jsonrpc

import (
	"encoding/json"
	"testing"
)

// An id is data, not source: one holding a quote must still encode as JSON.
func TestRequestIDIsEscaped(t *testing.T) {
	m, err := Request(`a"b\c`, "x.y", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encode(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var id string
	if err := json.Unmarshal(m.ID, &id); err != nil || id != `a"b\c` {
		t.Fatalf("id = %s (%v), want the string back", m.ID, err)
	}
	if _, err := Decode(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
