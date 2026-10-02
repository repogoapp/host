package hostlink

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/securechan"
	"github.com/repogo/host/internal/wsconn"
)

// A handshake left after its first message is forgotten, and so is its peer
// unless a session is live.
func TestSweepForgetsAbandonedHandshakes(t *testing.T) {
	now := time.Now()
	live := &channel{cancel: func() {}}
	c := &conn{peers: map[device.ID]*peer{
		"abandoned": {hsStarted: now.Add(-handshakeTTL - time.Second), hs: new(securechan.Responder)},
		"fresh":     {hsStarted: now, hs: new(securechan.Responder)},
		"rekeying":  {hsStarted: now.Add(-handshakeTTL - time.Second), hs: new(securechan.Responder), ch: live},
		"idle":      {ch: live},
	}}
	c.sweep(now)

	if _, ok := c.peers["abandoned"]; ok {
		t.Error("an abandoned handshake was kept")
	}
	if p := c.peers["fresh"]; p == nil || p.hs == nil {
		t.Error("a handshake in progress was swept")
	}
	if p := c.peers["rekeying"]; p == nil || p.hs != nil || p.ch != live {
		t.Error("a stale rekey should drop its handshake and keep the session")
	}
	if p := c.peers["idle"]; p == nil || p.ch != live {
		t.Error("a live session was swept")
	}
}

// A duplicate control reply must not wedge the read loop it arrives on.
func TestADuplicateControlReplyDoesNotBlock(t *testing.T) {
	c := &conn{control: map[string]chan *jsonrpc.Message{"1": make(chan *jsonrpc.Message, 1)}}
	reply := []byte(`{"jsonrpc":"2.0","id":"1","result":{"ok":true}}`)
	done := make(chan struct{})
	go func() {
		c.controlReply(reply)
		c.controlReply(reply)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a duplicate control reply blocked the read loop")
	}
}

// sink is a socket whose far end reads and discards, for a conn whose
// writes are not under test.
func sink(t *testing.T) *wsconn.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		for {
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	ws, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return wsconn.Wrap(ws)
}

// message1 starts a handshake as phone and abandons it after message 1,
// returning that framed message.
func message1(t *testing.T, phone, host *device.Identity) []byte {
	t.Helper()
	var msg1 []byte
	abandon := errors.New("abandoned after message 1")
	_, err := securechan.Initiate(t.Context(), phone, host.ID, host.Public,
		func(_ context.Context, b []byte) error { msg1 = b; return nil },
		func(context.Context) ([]byte, error) { return nil, abandon })
	if !errors.Is(err, abandon) || len(msg1) != securechan.Message1Len {
		t.Fatalf("message 1: %d bytes, %v", len(msg1), err)
	}
	return msg1
}

// A phone that starts over while the host awaits its message 3 is told apart
// by message 1's length and gets a fresh handshake, not a failed Finish.
func TestAPhoneStartingOverGetsAFreshHandshake(t *testing.T) {
	phone, host := generateIdentity(t), generateIdentity(t)
	c := &conn{ws: sink(t), identity: host, base: t.Context(), peers: map[device.ID]*peer{}}

	if _, err := c.handshake(t.Context(), phone.ID, message1(t, phone, host)); err != nil {
		t.Fatal(err)
	}
	first := c.peers[phone.ID].hs
	if first == nil {
		t.Fatal("no handshake after message 1")
	}
	if _, err := c.handshake(t.Context(), phone.ID, message1(t, phone, host)); err != nil {
		t.Fatalf("a second message 1 was taken for message 3: %v", err)
	}
	if p := c.peers[phone.ID]; p == nil || p.hs == nil || p.hs == first || p.ch != nil {
		t.Fatal("the second message 1 did not start a fresh handshake")
	}
}

func generateIdentity(t *testing.T) *device.Identity {
	t.Helper()
	id, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
