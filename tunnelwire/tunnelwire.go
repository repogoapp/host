// Package tunnelwire is the message format between a host and the tunnel
// gateway: one WebSocket binary message per frame, laid out as
// [kind][u32 header length][JSON header][raw bytes].
package tunnelwire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// ConnectPath is where a host opens its WebSocket on the gateway.
const ConnectPath = "/connect"

// MaxMessage bounds one frame either way: a buffered body plus its header.
const MaxMessage = 64 << 20

// ProofMessage is what the host signs to answer a challenge: plain bytes, so
// no encoding has to be canonical.
func ProofMessage(nonce []byte, gatewayID string, timeMS uint64) []byte {
	msg := []byte("repogo-tunnel/1|")
	msg = append(msg, nonce...)
	msg = append(msg, '|')
	msg = append(msg, gatewayID...)
	return fmt.Appendf(append(msg, '|'), "%d", timeMS)
}

// Kind is a frame's first byte.
type Kind byte

const (
	KindHello         Kind = 1 // host → gateway
	KindChallenge     Kind = 2 // gateway → host
	KindProof         Kind = 3 // host → gateway
	KindReady         Kind = 4 // gateway → host
	KindRequest       Kind = 5 // gateway → host
	KindResponse      Kind = 6 // host → gateway
	KindBodyChunk     Kind = 7 // host → gateway
	KindWSOpen        Kind = 8 // gateway → host
	KindWSOpenAck     Kind = 9 // host → gateway
	KindWSFrame       Kind = 10
	KindWSClose       Kind = 11
	KindPluginRequest Kind = 12 // gateway → host
)

// Message is any frame's body.
type Message interface{ Kind() Kind }

type Hello struct {
	PublicKey []byte `json:"public_key"`
}

type Challenge struct {
	Nonce     []byte `json:"nonce"`
	GatewayID string `json:"gateway_id"`
	Time      uint64 `json:"time"` // Unix ms
}

type Proof struct {
	Signature []byte `json:"signature"`
}

type Ready struct{}

// Request is one visitor's HTTP request; Body travels as the raw bytes.
type Request struct {
	SessionID   string   `json:"session_id"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Target      string   `json:"target"` // the local port
	HeaderLines []string `json:"header_lines"`
	Body        []byte   `json:"-"`
}

// PluginRequest is a caller's HTTP request to the environment's own address,
// h-<host id>.<domain>. Its own kind, so it can only reach plugins, never a port.
type PluginRequest struct {
	SessionID   string   `json:"session_id"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	HeaderLines []string `json:"header_lines"`
	Body        []byte   `json:"-"`
}

// Response answers a Request or a PluginRequest; Streaming means BodyChunks follow.
type Response struct {
	SessionID   string   `json:"session_id"`
	Status      int      `json:"status"`
	HeaderLines []string `json:"header_lines"`
	Streaming   bool     `json:"streaming,omitempty"`
	Body        []byte   `json:"-"`
}

type BodyChunk struct {
	SessionID   string `json:"session_id"`
	EndOfStream bool   `json:"end_of_stream,omitempty"`
	Data        []byte `json:"-"`
}

type WSOpen struct {
	SessionID    string   `json:"session_id"`
	Target       string   `json:"target"`
	Path         string   `json:"path"`
	HeaderLines  []string `json:"header_lines"`
	Subprotocols []string `json:"subprotocols,omitempty"`
}

// WSOpenAck reports the local socket; a CloseCode other than 0 means it failed.
type WSOpenAck struct {
	SessionID   string `json:"session_id"`
	Subprotocol string `json:"subprotocol,omitempty"`
	CloseCode   int    `json:"close_code,omitempty"`
	CloseReason string `json:"close_reason,omitempty"`
}

type WSFrame struct {
	SessionID string `json:"session_id"`
	Binary    bool   `json:"binary,omitempty"`
	Data      []byte `json:"-"`
}

type WSClose struct {
	SessionID string `json:"session_id"`
	Code      int    `json:"code"`
	Reason    string `json:"reason,omitempty"`
}

func (*Hello) Kind() Kind         { return KindHello }
func (*Challenge) Kind() Kind     { return KindChallenge }
func (*Proof) Kind() Kind         { return KindProof }
func (*Ready) Kind() Kind         { return KindReady }
func (*Request) Kind() Kind       { return KindRequest }
func (*Response) Kind() Kind      { return KindResponse }
func (*BodyChunk) Kind() Kind     { return KindBodyChunk }
func (*WSOpen) Kind() Kind        { return KindWSOpen }
func (*WSOpenAck) Kind() Kind     { return KindWSOpenAck }
func (*WSFrame) Kind() Kind       { return KindWSFrame }
func (*WSClose) Kind() Kind       { return KindWSClose }
func (*PluginRequest) Kind() Kind { return KindPluginRequest }

// raw is the part of a message that travels unencoded after the header.
func raw(m Message) []byte {
	switch m := m.(type) {
	case *Request:
		return m.Body
	case *PluginRequest:
		return m.Body
	case *Response:
		return m.Body
	case *BodyChunk:
		return m.Data
	case *WSFrame:
		return m.Data
	}
	return nil
}

// Encode lays out one frame.
func Encode(m Message) ([]byte, error) {
	header, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	body := raw(m)
	out := make([]byte, 5, 5+len(header)+len(body))
	out[0] = byte(m.Kind())
	binary.BigEndian.PutUint32(out[1:], uint32(len(header)))
	return append(append(out, header...), body...), nil
}

// Decode reads one frame. The raw bytes alias b.
func Decode(b []byte) (Message, error) {
	if len(b) < 5 {
		return nil, errors.New("tunnelwire: short frame")
	}
	n := binary.BigEndian.Uint32(b[1:])
	if uint64(len(b)-5) < uint64(n) {
		return nil, errors.New("tunnelwire: header overruns frame")
	}
	header, body := b[5:5+n], b[5+n:]
	var m Message
	switch Kind(b[0]) {
	case KindHello:
		m = &Hello{}
	case KindChallenge:
		m = &Challenge{}
	case KindProof:
		m = &Proof{}
	case KindReady:
		m = &Ready{}
	case KindRequest:
		m = &Request{Body: body}
	case KindPluginRequest:
		m = &PluginRequest{Body: body}
	case KindResponse:
		m = &Response{Body: body}
	case KindBodyChunk:
		m = &BodyChunk{Data: body}
	case KindWSOpen:
		m = &WSOpen{}
	case KindWSOpenAck:
		m = &WSOpenAck{}
	case KindWSFrame:
		m = &WSFrame{Data: body}
	case KindWSClose:
		m = &WSClose{}
	default:
		return nil, fmt.Errorf("tunnelwire: unknown kind %d", b[0])
	}
	if err := json.Unmarshal(header, m); err != nil {
		return nil, fmt.Errorf("tunnelwire: kind %d header: %w", b[0], err)
	}
	return m, nil
}
