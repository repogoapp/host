package tunnel

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/internal/testwait"
	"github.com/repogoapp/host/tunnelwire"
)

// await is the next message from the host that match accepts; others are dropped.
func (c *fakeConn) await(t *testing.T, what string, match func(tunnelwire.Message) bool) tunnelwire.Message {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-c.in:
			if !ok {
				t.Fatalf("socket closed waiting for %s", what)
			}
			if match(m) {
				return m
			}
		case <-timeout:
			t.Fatalf("no %s from the host", what)
		}
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// listen serves h on loopback and answers its port.
func listen(t *testing.T, h http.Handler) int {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return n
}

// The host says it reads HTTPCancel, so a gateway may send one.
func TestHelloSaysItCancels(t *testing.T) {
	g := newFakeGateway(t, "127.0.0.1")
	s, _ := newService(t, g.addr, peers{phone: {ID: phone}})
	run(t, s)
	if _, err := s.Open(phone, "abcdefghij12", 4000, inAnHour()); err != nil {
		t.Fatal(err)
	}
	if c := g.accept(t); !c.cancels {
		t.Fatal("Hello left CancelsHTTP unset")
	}
}

// A canceled session ends the local request it started, and gives back its place.
func TestCancelEndsTheLocalRequest(t *testing.T) {
	g := newFakeGateway(t, "127.0.0.1")
	s, _ := newService(t, g.addr, peers{phone: {ID: phone}})
	started, ended := make(chan struct{}), make(chan struct{})
	port := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(ended)
	}))
	run(t, s)
	if _, err := s.Open(phone, "abcdefghij12", port, inAnHour()); err != nil {
		t.Fatal(err)
	}
	c := g.accept(t)
	req := &tunnelwire.Request{SessionID: "s1", Method: "GET", Path: "/hangs", Target: strconv.Itoa(port), HeaderLines: []string{"Host: abcdefghij12." + Domain}}
	if err := c.send(req); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, started, "the local request")
	if err := c.send(&tunnelwire.HTTPCancel{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, ended, "the local request to end")
	testwait.For(t, "the request's place back", func() bool { return len(s.httpSlots) == 0 })
}

// A host at its limit answers 503 at once rather than queueing the request.
func TestBusyHostRefusesAtOnce(t *testing.T) {
	g := newFakeGateway(t, "127.0.0.1")
	s, _ := newService(t, g.addr, peers{phone: {ID: phone}})
	port := devServer(t, "ok")
	run(t, s)
	if _, err := s.Open(phone, "abcdefghij12", port, inAnHour()); err != nil {
		t.Fatal(err)
	}
	c := g.accept(t)
	for range cap(s.httpSlots) {
		s.httpSlots.take()
	}
	status, header, _ := c.get(t, "full", strconv.Itoa(port), "abcdefghij12."+Domain)
	if status != http.StatusServiceUnavailable || header.Get("X-Repogo-Gateway-Error") != "Computer busy" {
		t.Fatalf("at the limit: %d %v", status, header)
	}
	s.httpSlots.give()
	if status, _, body := c.get(t, "room", strconv.Itoa(port), "abcdefghij12."+Domain); status != http.StatusOK || body != "ok" {
		t.Fatalf("with room: %d %q", status, body)
	}
}

// heldPlugins stands in for Bun: canceled, it still answers only when released.
type heldPlugins struct {
	started, canceled, release chan struct{}
}

func (p *heldPlugins) HasRoutes() bool { return true }

func (p *heldPlugins) ServeHTTP(ctx context.Context, _ tunnelwire.PluginRoute, _ *http.Request) (*http.Response, error) {
	close(p.started)
	<-ctx.Done()
	close(p.canceled)
	<-p.release
	return nil, ctx.Err()
}

// A canceled plugin call keeps its place until the plugin has answered.
func TestPluginPlaceHeldUntilThePluginAnswers(t *testing.T) {
	g := newFakeGateway(t, "127.0.0.1")
	s, id := newService(t, g.addr, peers{})
	plugins := &heldPlugins{started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	s.cfg.Plugins = plugins
	run(t, s)
	c := g.accept(t)
	host := "h-" + string(id.ID) + "." + Domain
	if err := c.send(&tunnelwire.PluginRequest{SessionID: "p1", Method: "POST", Path: "/deploys/notify", HeaderLines: []string{"Host: " + host}}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, plugins.started, "the plugin call")
	if err := c.send(&tunnelwire.HTTPCancel{SessionID: "p1"}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, plugins.canceled, "the call's context to end")
	if n := len(s.pluginSlots); n != 1 {
		t.Fatalf("%d places taken while the plugin still runs, want 1", n)
	}
	close(plugins.release)
	testwait.For(t, "the call's place back", func() bool { return len(s.pluginSlots) == 0 })
}

// A local socket that stops reading is dropped without holding up the
// gateway socket's reader, which goes on serving other sessions.
func TestStalledLocalSocketDoesNotBlockTheReader(t *testing.T) {
	g := newFakeGateway(t, "127.0.0.1")
	s, _ := newService(t, g.addr, peers{phone: {ID: phone}})
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	stalled := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		// Never reads, so the host's writes back up.
		<-stop
	}))
	served := devServer(t, "ok")
	run(t, s)
	if _, err := s.Open(phone, "abcdefghij12", stalled, inAnHour()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(phone, "klmnopqrst34", served, inAnHour()); err != nil {
		t.Fatal(err)
	}
	c := g.accept(t)
	open := &tunnelwire.WSOpen{SessionID: "w1", Target: strconv.Itoa(stalled), Path: "/", HeaderLines: []string{"Host: abcdefghij12." + Domain}}
	if err := c.send(open); err != nil {
		t.Fatal(err)
	}
	ack := c.await(t, "an ack", func(m tunnelwire.Message) bool { _, ok := m.(*tunnelwire.WSOpenAck); return ok }).(*tunnelwire.WSOpenAck)
	if ack.CloseCode != 0 {
		t.Fatalf("ack %+v", ack)
	}
	// Far more than the queue and the kernel's buffers hold.
	frame := bytes.Repeat([]byte{1}, 256<<10)
	for range 4 * wsQueue {
		if err := c.send(&tunnelwire.WSFrame{SessionID: "w1", Binary: true, Data: frame}); err != nil {
			t.Fatal(err)
		}
	}
	c.await(t, "the stalled socket's close", func(m tunnelwire.Message) bool {
		cl, ok := m.(*tunnelwire.WSClose)
		return ok && cl.SessionID == "w1"
	})
	if status, _, body := c.get(t, "after", strconv.Itoa(served), "klmnopqrst34."+Domain); status != http.StatusOK || body != "ok" {
		t.Fatalf("after the stall: %d %q", status, body)
	}
}
