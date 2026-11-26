package tunnelgateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/tunnelwire"
)

// A host can't make Fly's proxy send the visitor's request to another app.
func TestHostCannotSteerFlysProxy(t *testing.T) {
	w := httptest.NewRecorder()
	writeResponseHeaders(w, []string{
		"Fly-Replay: app=other",
		"fly-replay-src: x",
		"Content-Type: application/vnd.fly.replay+json",
		"X-Kept: 1",
	})
	if got := w.Header().Get("Fly-Replay") + w.Header().Get("Fly-Replay-Src"); got != "" {
		t.Fatalf("fly headers passed: %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("content type %q", got)
	}
	if w.Header().Get("X-Kept") != "1" {
		t.Fatal("ordinary header dropped")
	}
}

// A socket that hasn't proven a key can't send a large message.
func TestUnprovenSocketIsCappedSmall(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.url, "http")+tunnelwire.ConnectPath, &websocket.DialOptions{Host: gatewayID})
	if err != nil {
		t.Fatal(err)
	}
	h := &testHost{conn: conn, ctx: ctx}
	pub, _, _ := ed25519.GenerateKey(nil)
	_ = h.send(&tunnelwire.Hello{PublicKey: pub})
	if _, err := h.read(); err != nil {
		t.Fatal(err)
	}
	_ = h.send(&tunnelwire.Proof{Signature: bytes.Repeat([]byte{1}, proofReadLimit)})
	_, err = h.read()
	if got := websocket.CloseStatus(err); got != websocket.StatusMessageTooBig {
		t.Fatalf("closed with %v (%v), want message too big", got, err)
	}
}

func TestSocketsPerAddressAreCapped(t *testing.T) {
	var c socketCounts
	for range socketsPerIP {
		if !c.take("1.2.3.4") {
			t.Fatal("refused below the cap")
		}
	}
	if c.take("1.2.3.4") {
		t.Fatal("took one past the cap")
	}
	if !c.take("5.6.7.8") {
		t.Fatal("another address was refused")
	}
	c.give("1.2.3.4")
	if !c.take("1.2.3.4") {
		t.Fatal("a freed place wasn't given back")
	}
}

// Once the gateway holds its budget of request bodies, uploads wait their turn.
func TestBodyBudgetRefusesUploads(t *testing.T) {
	r := newRig(t)
	h := r.host("ok")
	r.row("abcdefghij12", Tunnel{HostID: h.id, Target: "3000", Enabled: true, ExpiresAt: inAnHour()})
	if !r.server.bodies.take(bodyBudgetBytes) {
		t.Fatal("budget not empty")
	}
	resp := r.do(http.MethodPost, "abcdefghij12."+domain, strings.NewReader("hello"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
	r.server.bodies.give(bodyBudgetBytes)
	resp = r.do(http.MethodPost, "abcdefghij12."+domain, strings.NewReader("hello"))
	if body := read(t, resp); resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if r.server.bodies.used != 0 {
		t.Fatalf("%d bytes still held", r.server.bodies.used)
	}
}

// A chunk larger than any a host sends ends its session.
func TestOversizedChunkEndsTheSession(t *testing.T) {
	h, sent := newTestHost(true)
	done := start(context.Background(), h, "s1")
	sent.next(t)
	h.Dispatch(&tunnelwire.Response{SessionID: "s1", Status: 200, Streaming: true})
	r := wait(t, done)
	h.Dispatch(&tunnelwire.BodyChunk{SessionID: "s1", Data: make([]byte, maxChunk+1)})
	if c, ok := sent.next(t).(*tunnelwire.HTTPCancel); !ok || c.SessionID != "s1" {
		t.Fatalf("sent %+v, want HTTPCancel s1", c)
	}
	for range r.res.Chunks {
		t.Fatal("the oversized chunk was passed on")
	}
}

func (r *rig) lookupCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookups
}

// A name that can't be a tunnel's, or that just missed, costs no lookup.
func TestMadeUpNamesCostNoLookup(t *testing.T) {
	r := newRig(t)
	for _, name := range []string{"short", "has_underscore1", "has-hyphen12"} {
		if resp := r.do(http.MethodGet, name+"."+domain, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if n := r.lookupCount(); n != 0 {
		t.Fatalf("%d lookups for impossible names", n)
	}
	for range 3 {
		if resp := r.do(http.MethodGet, "nosuchslug12."+domain, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	if n := r.lookupCount(); n != 1 {
		t.Fatalf("%d lookups for one missing name, want 1", n)
	}
}

// The per-address limit holds before any lookup, and refused requests
// don't count against a link.
func TestAddressLimitComesFirst(t *testing.T) {
	r := newRig(t)
	r.server.Limits = NewRateLimits(100, 2, time.Hour)
	for i, want := range []int{http.StatusNotFound, http.StatusNotFound, http.StatusTooManyRequests} {
		name := fmt.Sprintf("missing%05d", i)
		if resp := r.do(http.MethodGet, name+"."+domain, nil); resp.StatusCode != want {
			t.Fatalf("request %d: %d, want %d", i, resp.StatusCode, want)
		}
	}
	if n := r.lookupCount(); n != 2 {
		t.Fatalf("%d lookups, want 2", n)
	}
}

func TestRefusedRequestsAreNotCounted(t *testing.T) {
	l := NewRateLimits(1, 1, time.Hour)
	now := time.Now()
	if !l.AllowSlug("a", now) || l.AllowSlug("a", now) || l.AllowSlug("a", now) {
		t.Fatal("limit of one not held")
	}
	if l.counts["s:a"] != 1 {
		t.Fatalf("counted %d, want 1", l.counts["s:a"])
	}
}

func TestMissCacheStaysSmall(t *testing.T) {
	var c missCache
	now := time.Now()
	for i := range maxMisses + 10 {
		c.add(fmt.Sprintf("name%d", i), now)
	}
	if n := len(c.until); n > maxMisses {
		t.Fatalf("%d names held", n)
	}
	if c.recent("name1", now.Add(missFor)) {
		t.Fatal("a miss outlived missFor")
	}
}
