package relay

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/testwait"
)

// dialRaw opens a socket without the handshake, which is enough to hold a slot.
func (h *harness) dialRaw(header http.Header) (*websocket.Conn, *http.Response, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, h.url, &websocket.DialOptions{HTTPHeader: header})
	if err == nil {
		h.t.Cleanup(func() { ws.CloseNow() })
	}
	return ws, resp, err
}

func refused(resp *http.Response, err error) bool {
	return err != nil && resp != nil && resp.StatusCode == http.StatusTooManyRequests
}

// One address holds a bounded number of sockets, handshake or not, and gets
// its slot back when one closes.
func TestConnectionsPerAddressAreCapped(t *testing.T) {
	h := newHarnessWith(t, Config{MaxConnsPerIP: 2})
	first, _, err := h.dialRaw(nil)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, _, err := h.dialRaw(nil); err != nil {
		t.Fatalf("second: %v", err)
	}
	if _, resp, err := h.dialRaw(nil); !refused(resp, err) {
		t.Fatalf("third socket from one address was not refused: %v", err)
	}

	first.CloseNow()
	testwait.For(t, "the closed socket's slot", func() bool {
		ws, _, err := h.dialRaw(nil)
		if err != nil {
			return false
		}
		ws.CloseNow()
		return true
	})
}

// Behind a proxy every socket comes from the proxy; the trusted header is the
// caller.
func TestClientIPHeaderIsTheAddress(t *testing.T) {
	h := newHarnessWith(t, Config{MaxConnsPerIP: 1, ClientIPHeader: "Fly-Client-IP"})
	from := func(ip string) http.Header { return http.Header{"Fly-Client-IP": {ip}} }

	if _, _, err := h.dialRaw(from("203.0.113.1")); err != nil {
		t.Fatalf("first address: %v", err)
	}
	if _, _, err := h.dialRaw(from("203.0.113.2")); err != nil {
		t.Fatalf("a second address shared the first one's cap: %v", err)
	}
	if _, resp, err := h.dialRaw(from("203.0.113.1")); !refused(resp, err) {
		t.Fatalf("the first address exceeded its cap: %v", err)
	}
}

// A sender over its rate is slowed, never dropped: every frame arrives, in
// order, and the flood takes as long as its rate allows.
func TestFloodIsPacedNotDropped(t *testing.T) {
	h := newHarnessWith(t, Config{}, func(s *Server) {
		s.limits.messages, s.limits.messageBurst = 20, 5
	})
	host, _ := h.connect("group-a", handshake.RoleRuntime)
	phone, _ := h.connect("group-a", handshake.RoleClient)

	const n = 25
	start := time.Now()
	// Tiny frames: the socket buffers them all, so sending never blocks.
	for i := range n {
		phone.send(host.id, fmt.Sprint(i))
	}
	for i := range n {
		_, body, err := host.recv(5 * time.Second)
		if err != nil {
			t.Fatalf("frame %d never arrived: %v", i, err)
		}
		if body != fmt.Sprint(i) {
			t.Fatalf("frame %d arrived as %q", i, body)
		}
	}
	// (25 - 5 burst) / 20 per second = 1s.
	if elapsed := time.Since(start); elapsed < 800*time.Millisecond {
		t.Fatalf("25 frames at 20/s took %v; the flood was not paced", elapsed)
	}
}

// A frame larger than the burst waits for a full bucket rather than forever.
func TestBucketTakesOversizedFrames(t *testing.T) {
	b := newBucket(1000, 10)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for range 3 {
		if err := b.wait(ctx, 1e9); err != nil {
			t.Fatalf("an oversized frame never went through: %v", err)
		}
	}
}
