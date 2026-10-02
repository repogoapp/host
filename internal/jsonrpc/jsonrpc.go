// Package jsonrpc is the JSON-RPC 2.0 wire format for every host <-> client
// message; server pushes are notifications.
package jsonrpc

import (
	"encoding/json"
	"fmt"
)

const Version = "2.0"

// MaxMessageBytes bounds one message so a hostile peer cannot allocate without limit.
const MaxMessageBytes = 8 << 20

// Message is every frame on the wire; the spec distinguishes kinds by which
// fields are present.
type Message struct {
	JSONRPC string `json:"jsonrpc"`

	// Echoed verbatim: the spec allows a string, a number or null.
	ID json.RawMessage `json:"id,omitempty"`

	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`

	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

func (m *Message) IsRequest() bool      { return m.Method != "" && len(m.ID) > 0 }
func (m *Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message) }

// The spec's codes, then ours in the block the spec reserves for applications.
const (
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603

	CodeDenied      = -32000
	CodeNotFound    = -32001
	CodeUnavailable = -32002
)

func Request(id string, method string, params any) (*Message, error) {
	raw, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	return withParams(&Message{JSONRPC: Version, ID: raw, Method: method}, params)
}

func Notify(method string, params any) (*Message, error) {
	return withParams(&Message{JSONRPC: Version, Method: method}, params)
}

func Result(id json.RawMessage, result any) (*Message, error) {
	b, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &Message{JSONRPC: Version, ID: id, Result: b}, nil
}

func Fail(id json.RawMessage, code int, msg string) *Message {
	return &Message{JSONRPC: Version, ID: id, Error: &Error{Code: code, Message: msg}}
}

func withParams(m *Message, params any) (*Message, error) {
	if params == nil {
		return m, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	m.Params = b
	return m, nil
}

// Into decodes params or a result; an absent value is legitimate, not an error.
func Into(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func Encode(m *Message) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode message: %w", err)
	}
	if len(b) > MaxMessageBytes {
		return nil, fmt.Errorf("encode message: %d bytes exceeds limit %d", len(b), MaxMessageBytes)
	}
	return b, nil
}

func Decode(b []byte) (*Message, error) {
	if len(b) > MaxMessageBytes {
		return nil, fmt.Errorf("decode message: %d bytes exceeds limit %d", len(b), MaxMessageBytes)
	}
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	// Strict on the envelope, tolerant inside it. A peer that gets this wrong is
	// not speaking our protocol at all, and guessing would hide that.
	if m.JSONRPC != Version {
		return nil, fmt.Errorf("decode message: jsonrpc = %q, want %q", m.JSONRPC, Version)
	}
	return &m, nil
}
