package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/tunnelwire"
)

// fakeGateway is the tunnel gateway's host handshake and nothing else: it
// challenges, checks the proof as the gateway does, and hands the socket to
// the test, which then plays the gateway's side of requests.
type fakeGateway struct {
	id    string // the gateway_id it challenges with
	conns chan *fakeConn
	addr  string
}

type fakeConn struct {
	ctx    context.Context
	conn   *websocket.Conn
	hostID device.ID
	in     chan tunnelwire.Message
}

func newFakeGateway(t *testing.T, id string) *fakeGateway {
	t.Helper()
	g := &fakeGateway{id: id, conns: make(chan *fakeConn, 4)}
	srv := httptest.NewServer(http.HandlerFunc(g.connect))
	t.Cleanup(srv.Close)
	g.addr = srv.Listener.Addr().String()
	return g
}

func (g *fakeGateway) connect(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != tunnelwire.ConnectPath {
		http.NotFound(w, r)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(tunnelwire.MaxMessage)
	c := &fakeConn{ctx: r.Context(), conn: conn, in: make(chan tunnelwire.Message, 64)}
	hello, ok := c.read().(*tunnelwire.Hello)
	if !ok || len(hello.PublicKey) != ed25519.PublicKeySize {
		return
	}
	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	now := uint64(time.Now().UnixMilli())
	if c.send(&tunnelwire.Challenge{Nonce: nonce, GatewayID: g.id, Time: now}) != nil {
		return
	}
	proof, ok := c.read().(*tunnelwire.Proof)
	// Built here byte by byte, independently of ProofMessage.
	msg := []byte("repogo-tunnel/1|" + string(nonce) + "|" + g.id + "|" + strconv.FormatUint(now, 10))
	if !ok || !ed25519.Verify(hello.PublicKey, msg, proof.Signature) {
		return
	}
	if c.send(&tunnelwire.Ready{}) != nil {
		return
	}
	c.hostID = device.IDFor(hello.PublicKey)
	g.conns <- c
	defer close(c.in)
	for {
		m := c.read()
		if m == nil {
			return
		}
		c.in <- m
	}
}

// read is the next message, or nil once the socket ends.
func (c *fakeConn) read() tunnelwire.Message {
	_, b, err := c.conn.Read(c.ctx)
	if err != nil {
		return nil
	}
	m, err := tunnelwire.Decode(b)
	if err != nil {
		return nil
	}
	return m
}

func (c *fakeConn) send(m tunnelwire.Message) error {
	b, err := tunnelwire.Encode(m)
	if err != nil {
		return err
	}
	return c.conn.Write(c.ctx, websocket.MessageBinary, b)
}

func (g *fakeGateway) accept(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case c := <-g.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the host never connected")
		return nil
	}
}

// get sends one request as the gateway would and gathers the reply, streamed or not.
func (c *fakeConn) get(t *testing.T, id, target, host string) (int, http.Header, string) {
	t.Helper()
	lines := []string{"Accept: text/plain", "X-Forwarded-Host: forged.example"}
	if host != "" {
		lines = append(lines, "Host: "+host)
	}
	if err := c.send(&tunnelwire.Request{SessionID: id, Method: "GET", Path: "/hello", Target: target, HeaderLines: lines}); err != nil {
		t.Fatal(err)
	}
	var status int
	header := http.Header{}
	var body strings.Builder
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-c.in:
			if !ok {
				t.Fatal("stream closed")
			}
			switch m := m.(type) {
			case *tunnelwire.Response:
				if m.SessionID != id {
					continue
				}
				status = m.Status
				for _, l := range m.HeaderLines {
					k, v, _ := strings.Cut(l, ": ")
					header.Add(k, v)
				}
				body.Write(m.Body)
				if !m.Streaming {
					return status, header, body.String()
				}
			case *tunnelwire.BodyChunk:
				if m.SessionID != id {
					continue
				}
				body.Write(m.Data)
				if m.EndOfStream {
					return status, header, body.String()
				}
			}
		case <-timeout:
			t.Fatal("no reply")
		}
	}
}

type peers map[device.ID]device.Peer

func (p peers) Peer(id device.ID) (device.Peer, error) {
	peer, ok := p[id]
	if !ok {
		return device.Peer{}, device.ErrUnknownDevice
	}
	return peer, nil
}

const phone device.ID = "0123456789abcdef0123456789abcdef"

func newService(t *testing.T, gatewayAddr string, ps peers) (*Service, *device.Identity) {
	t.Helper()
	id, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{
		Path: filepath.Join(t.TempDir(), "tunnels.json"), Identity: id, Peers: ps,
		Gateway: gatewayAddr, Plaintext: true, Changed: func(Changed) {}, OwnPort: func() int { return ownPort }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, id
}

func devServer(t *testing.T, body string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Forwarded-Host", r.Header.Get("X-Forwarded-Host"))
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return n
}

func run(t *testing.T, s *Service) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	// As in the host, Run is up before anything opens a tunnel.
	for {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if running {
			return
		}
		runtime.Gosched()
	}
}

func inAnHour() int64 { return time.Now().Add(time.Hour).UnixMilli() }

func TestServesOnlyWhatItOpened(t *testing.T) {
	g := newFakeGateway(t, "127.0.0.1")
	ps := peers{phone: {ID: phone}}
	s, id := newService(t, g.addr, ps)
	shared := devServer(t, "shared")
	unshared := devServer(t, "private")
	run(t, s)

	if _, err := s.Open(phone, "abcdefghij12", shared, inAnHour()); err != nil {
		t.Fatal(err)
	}
	c := g.accept(t)
	if c.hostID != id.ID {
		t.Fatalf("gateway saw host %s, want %s", c.hostID, id.ID)
	}

	status, header, body := c.get(t, "s1", strconv.Itoa(shared), "abcdefghij12.repogo.dev")
	if status != 200 || body != "shared" {
		t.Fatalf("open tunnel: %d %q", status, body)
	}
	if got := header.Get("X-Seen-Forwarded-Host"); got != "abcdefghij12.repogo.dev" {
		t.Fatalf("dev server saw X-Forwarded-Host %q", got)
	}

	for name, req := range map[string][2]string{
		"a port this host never shared":   {strconv.Itoa(unshared), "abcdefghij12.repogo.dev"},
		"a LAN target":                    {"192.168.1.2:" + strconv.Itoa(shared), "abcdefghij12.repogo.dev"},
		"a loopback name":                 {"localhost:" + strconv.Itoa(shared), "abcdefghij12.repogo.dev"},
		"a slug this host never opened":   {strconv.Itoa(shared), "zzzzzzzzzz99.repogo.dev"},
		"another domain":                  {strconv.Itoa(shared), "abcdefghij12.example.com"},
		"a deeper name under the domain":  {strconv.Itoa(shared), "abcdefghij12.tunnels.repogo.dev"},
		"no Host line from the gateway":   {strconv.Itoa(shared), ""},
		"a Host line with an extra label": {strconv.Itoa(shared), "abcdefghij12.evil.repogo.dev"},
	} {
		status, _, body := c.get(t, name, req[0], req[1])
		if status != http.StatusForbidden || strings.Contains(body, "private") || body == "shared" {
			t.Errorf("%s: %d %q", name, status, body)
		}
	}

	// The phone that opened it is unpaired: the tunnel stops at once.
	delete(ps, phone)
	if status, _, _ := c.get(t, "s2", strconv.Itoa(shared), "abcdefghij12.repogo.dev"); status != http.StatusForbidden {
		t.Fatalf("revoked opener still served: %d", status)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("revoked opener's tunnel still listed: %v", got)
	}
}

func TestDialsOnlyWhileATunnelIsOpen(t *testing.T) {
	g := newFakeGateway(t, "127.0.0.1")
	s, _ := newService(t, g.addr, peers{phone: {ID: phone}})
	changes := make(chan Changed, 16)
	s.cfg.Changed = func(c Changed) { changes <- c }
	run(t, s)

	select {
	case <-g.conns:
		t.Fatal("dialled the gateway with nothing open")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := s.Open(phone, "abcdefghij12", 3000, inAnHour()); err != nil {
		t.Fatal(err)
	}
	c := g.accept(t)
	waitFor(t, changes, func(ev Changed) bool { return ev.Connected && len(ev.Tunnels) == 1 })

	if err := s.Close("abcdefghij12"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, changes, func(ev Changed) bool { return !ev.Connected && len(ev.Tunnels) == 0 })
	select {
	case _, ok := <-c.in:
		for ok {
			_, ok = <-c.in
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived the last tunnel")
	}
}

func waitFor(t *testing.T, ch chan Changed, ok func(Changed) bool) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ok(ev) {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for tunnels.changed")
		}
	}
}

// tunnels.open answers once the gateway serves the URL, so a link made from
// its reply works on the first try; a gateway that refuses doesn't hold it up.
func TestWaitConnected(t *testing.T) {
	g := newFakeGateway(t, "127.0.0.1")
	s, _ := newService(t, g.addr, peers{phone: {ID: phone}})
	run(t, s)
	if _, err := s.Open(phone, "abcdefghij12", 3000, inAnHour()); err != nil {
		t.Fatal(err)
	}
	if !s.WaitConnected(t.Context()) || !s.Status().Connected {
		t.Fatal("WaitConnected returned before the gateway stream was up")
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := lis.Addr().String()
	_ = lis.Close()
	down, _ := newService(t, refused, peers{phone: {ID: phone}})
	run(t, down)
	if _, err := down.Open(phone, "abcdefghij12", 3000, inAnHour()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if down.WaitConnected(t.Context()) {
		t.Fatal("connected to a gateway that refuses")
	}
	if waited := time.Since(start); waited >= connectWait {
		t.Fatalf("waited %v for a refusing gateway", waited)
	}
}

func TestRefusesAChallengeForAnotherGateway(t *testing.T) {
	g := newFakeGateway(t, "gateway.elsewhere")
	s, _ := newService(t, g.addr, peers{phone: {ID: phone}})
	run(t, s)
	if _, err := s.Open(phone, "abcdefghij12", 3000, inAnHour()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-g.conns:
		t.Fatal("host proved itself to a gateway it did not dial")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestOpenValidates(t *testing.T) {
	s, _ := newService(t, "127.0.0.1:1", peers{phone: {ID: phone}})
	for name, a := range map[string]struct {
		slug    string
		port    int
		expires int64
	}{
		"short slug":        {"abc", 3000, inAnHour()},
		"uppercase slug":    {"ABCDEFGHIJ12", 3000, inAnHour()},
		"dotted slug":       {"abcdefghij.x", 3000, inAnHour()},
		"privileged port":   {"abcdefghij12", 80, inAnHour()},
		"port too high":     {"abcdefghij12", 70000, inAnHour()},
		"already expired":   {"abcdefghij12", 3000, time.Now().Add(-time.Second).UnixMilli()},
		"beyond seven days": {"abcdefghij12", 3000, time.Now().Add(8 * 24 * time.Hour).UnixMilli()},
	} {
		if _, err := s.Open(phone, a.slug, a.port, a.expires); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := s.Close("abcdefghij12"); err != ErrNotFound {
		t.Fatalf("close unknown: %v", err)
	}
}

func TestAllowlistPersistsPrivately(t *testing.T) {
	s, id := newService(t, "127.0.0.1:1", peers{phone: {ID: phone}})
	if _, err := s.Open(phone, "abcdefghij12", 3000, inAnHour()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	again, err := Open(Config{Path: s.cfg.Path, Identity: id, Peers: peers{phone: {ID: phone}}, Changed: func(Changed) {}, OwnPort: s.cfg.OwnPort, Log: s.cfg.Log})
	if err != nil {
		t.Fatal(err)
	}
	got := again.List()
	if len(got) != 1 || got[0].Port != 3000 || got[0].URL != "https://abcdefghij12.repogo.dev" || again.tunnels["abcdefghij12"].OpenedBy != phone {
		t.Fatalf("reloaded %v", got)
	}
}

func TestClosePortClosesOnlyThatPort(t *testing.T) {
	s, _ := newService(t, "127.0.0.1:1", peers{phone: {ID: phone}})
	for slug, port := range map[string]int{"abcdefghij12": 3000, "abcdefghij34": 3000, "abcdefghij56": 4000} {
		if _, err := s.Open(phone, slug, port, inAnHour()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ClosePort(3000); err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 1 || got[0].Port != 4000 {
		t.Fatalf("left %v", got)
	}
}

// ownPort stands in for the host's loopback listener in tests.
const ownPort = 51999

// A tunnel to the host's own port would put its RPC on the internet.
func TestOpenRefusesTheHostsOwnPort(t *testing.T) {
	s, _ := newService(t, "127.0.0.1:1", peers{phone: {ID: phone}})
	if _, err := s.Open(phone, "abcdefghij12", ownPort, inAnHour()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Open(own port) = %v, want ErrInvalid", err)
	}
}

// A visitor's WebSocket reaches the dev server and frames flow both ways.
func TestBridgesWebSockets(t *testing.T) {
	g := newFakeGateway(t, "127.0.0.1")
	s, _ := newService(t, g.addr, peers{phone: {ID: phone}})
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"vite-hmr"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			typ, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if conn.Write(r.Context(), typ, append([]byte("echo:"), data...)) != nil {
				return
			}
		}
	}))
	t.Cleanup(echo.Close)
	_, portText, _ := net.SplitHostPort(echo.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	run(t, s)
	if _, err := s.Open(phone, "abcdefghij12", port, inAnHour()); err != nil {
		t.Fatal(err)
	}
	c := g.accept(t)

	next := func() tunnelwire.Message {
		t.Helper()
		select {
		case m := <-c.in:
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("no message from the host")
			return nil
		}
	}
	open := &tunnelwire.WSOpen{SessionID: "w1", Target: portText, Path: "/hmr", Subprotocols: []string{"vite-hmr"},
		HeaderLines: []string{"Host: abcdefghij12.repogo.dev"}}
	if err := c.send(open); err != nil {
		t.Fatal(err)
	}
	ack, ok := next().(*tunnelwire.WSOpenAck)
	if !ok || ack.CloseCode != 0 || ack.Subprotocol != "vite-hmr" {
		t.Fatalf("ack %+v", ack)
	}
	if err := c.send(&tunnelwire.WSFrame{SessionID: "w1", Binary: true, Data: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	frame, ok := next().(*tunnelwire.WSFrame)
	if !ok || !frame.Binary || string(frame.Data) != "echo:hi" {
		t.Fatalf("frame %+v", frame)
	}

	refused := &tunnelwire.WSOpen{SessionID: "w2", Target: portText, Path: "/", HeaderLines: []string{"Host: zzzzzzzzzz99.repogo.dev"}}
	if err := c.send(refused); err != nil {
		t.Fatal(err)
	}
	if ack, ok := next().(*tunnelwire.WSOpenAck); !ok || ack.CloseCode != int(websocket.StatusPolicyViolation) {
		t.Fatalf("unopened slug: %+v", ack)
	}
}
