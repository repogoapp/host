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

	"google.golang.org/grpc"

	"github.com/repogo/host/internal/device"
	gw "github.com/repogo/host/internal/tunnel/wire/gateway"
	pv "github.com/repogo/host/internal/tunnel/wire/preview"
)

// fakeGateway is the preview gateway's host handshake and nothing else: it
// challenges, checks the proof as hostproof.go does, and hands the stream to
// the test, which then plays the gateway's side of requests.
type fakeGateway struct {
	gw.UnimplementedPreviewGatewayServer
	id    string // the gateway_id it challenges with
	conns chan *fakeConn
	addr  string
}

type fakeConn struct {
	stream gw.PreviewGateway_ConnectServer
	hostID device.ID
	in     chan *gw.ClientMessage
}

func newFakeGateway(t *testing.T, id string) *fakeGateway {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGateway{id: id, conns: make(chan *fakeConn, 4), addr: lis.Addr().String()}
	srv := grpc.NewServer()
	gw.RegisterPreviewGatewayServer(srv, g)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return g
}

func (g *fakeGateway) Connect(stream gw.PreviewGateway_ConnectServer) error {
	hello, err := stream.Recv()
	if err != nil {
		return err
	}
	pub := ed25519.PublicKey(hello.GetHello().GetPublicKey())
	if len(pub) != ed25519.PublicKeySize {
		return io.ErrUnexpectedEOF
	}
	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	now := uint64(time.Now().UnixMilli())
	if err := stream.Send(&gw.ServerMessage{Body: &gw.ServerMessage_Challenge{Challenge: &gw.HostChallenge{Nonce: nonce, GatewayId: g.id, Time: now}}}); err != nil {
		return err
	}
	proof, err := stream.Recv()
	if err != nil {
		return err
	}
	// Built here byte by byte, independently of ProofMessage.
	msg := []byte("repogo-tunnel/1|" + string(nonce) + "|" + g.id + "|" + strconv.FormatUint(now, 10))
	if !ed25519.Verify(pub, msg, proof.GetProof().GetSignature()) {
		return io.ErrUnexpectedEOF
	}
	if err := stream.Send(&gw.ServerMessage{Body: &gw.ServerMessage_Ready{Ready: &gw.Ready{}}}); err != nil {
		return err
	}
	c := &fakeConn{stream: stream, hostID: device.IDFor(pub), in: make(chan *gw.ClientMessage, 64)}
	g.conns <- c
	for {
		m, err := stream.Recv()
		if err != nil {
			close(c.in)
			return err
		}
		c.in <- m
	}
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
	req := &pv.PreviewRequest{SessionId: id, Method: "GET", Path: "/hello", Target: target, HeaderLines: lines}
	if err := c.stream.Send(&gw.ServerMessage{Body: &gw.ServerMessage_HttpRequest{HttpRequest: req}}); err != nil {
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
			if r := m.GetHttpResponse(); r != nil && r.SessionId == id {
				status = int(r.Response.Status)
				for _, l := range r.Response.HeaderLines {
					k, v, _ := strings.Cut(l, ": ")
					header.Add(k, v)
				}
				body.Write(r.Response.Body)
				if !r.Response.Streaming {
					return status, header, body.String()
				}
			}
			if ch := m.GetHttpBodyChunk(); ch != nil && ch.SessionId == id {
				body.Write(ch.Data)
				if ch.EndOfStream {
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
	if len(got) != 1 || got[0].Port != 3000 || got[0].URL != "https://abcdefghij12.repogo.dev" || got[0].OpenedBy != phone {
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
