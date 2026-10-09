package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/testwait"
)

const testServerID = "test-relay"

type harness struct {
	t   *testing.T
	url string
	srv *Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, Config{})
}

// newHarnessWith starts a relay from cfg, filling in the address, server id
// and log.
func newHarnessWith(t *testing.T, cfg Config, tune ...func(*Server)) *harness {
	t.Helper()
	cfg.Addr = "127.0.0.1:0"
	cfg.ServerID = testServerID
	cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for _, f := range tune {
		f(srv)
	}
	addr, err := srv.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return &harness{t: t, url: fmt.Sprintf("ws://%s/ws", addr.(*net.TCPAddr).String()), srv: srv}
}

// gone waits until the relay has detached every socket of id.
func (h *harness) gone(id device.ID) {
	h.t.Helper()
	testwait.For(h.t, "the relay to detach "+string(id), func() bool {
		h.srv.mu.RLock()
		defer h.srv.mu.RUnlock()
		return len(h.srv.conns[id]) == 0
	})
}

type peer struct {
	t             *testing.T
	ws            *websocket.Conn
	id            device.ID
	pub           ed25519.PublicKey
	priv          ed25519.PrivateKey
	serverVersion string
}

// connect runs the full handshake and returns the peer, or the refusal. Raw
// rather than hostclient: these cases are hostile handshakes a client cannot produce.
func (h *harness) connect(group, role string, opts ...func(*handshake.Hello)) (*peer, *jsonrpc.Error) {
	h.t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.connectAs(pub, priv, group, role, opts...)
}

func (h *harness) connectAs(pub ed25519.PublicKey, priv ed25519.PrivateKey, group,
	role string, opts ...func(*handshake.Hello)) (*peer, *jsonrpc.Error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, h.url, nil)
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	h.t.Cleanup(func() { ws.CloseNow() })

	p := &peer{t: h.t, ws: ws, id: device.IDFor(pub), pub: pub, priv: priv}

	msg := p.readJSON(ctx)
	if msg.Method != handshake.MethodChallenge {
		h.t.Fatalf("expected a challenge, got %q", msg.Method)
	}
	var ch handshake.Challenge
	if err := jsonrpc.Into(msg.Params, &ch); err != nil {
		h.t.Fatalf("challenge: %v", err)
	}

	signed, err := device.ChallengeMessage(ch.Nonce, ch.ServerID, ch.WallMS)
	if err != nil {
		h.t.Fatal(err)
	}
	hello := &handshake.Hello{
		DeviceID:     string(p.id),
		PublicKey:    pub,
		GroupID:      group,
		Role:         role,
		Platform:     "test",
		ChallengeSig: ed25519.Sign(priv, signed),
	}
	for _, opt := range opts {
		opt(hello)
	}
	req, err := jsonrpc.Request("hello", handshake.MethodHello, hello)
	if err != nil {
		h.t.Fatal(err)
	}
	p.writeJSON(ctx, req)

	reply := p.readJSON(ctx)
	if reply.Error != nil {
		return nil, reply.Error
	}
	var accepted handshake.Accepted
	if err := jsonrpc.Into(reply.Result, &accepted); err != nil {
		h.t.Fatalf("hello reply: %v", err)
	}
	p.serverVersion = accepted.ServerVersion
	return p, nil
}

func (p *peer) readJSON(ctx context.Context) *jsonrpc.Message {
	p.t.Helper()
	typ, b, err := p.ws.Read(ctx)
	if err != nil {
		p.t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		p.t.Fatalf("handshake message was %v, want text", typ)
	}
	m, err := jsonrpc.Decode(b)
	if err != nil {
		p.t.Fatalf("decode: %v", err)
	}
	return m
}

func (p *peer) writeJSON(ctx context.Context, m *jsonrpc.Message) {
	p.t.Helper()
	b, err := jsonrpc.Encode(m)
	if err != nil {
		p.t.Fatalf("encode: %v", err)
	}
	if err := p.ws.Write(ctx, websocket.MessageText, b); err != nil {
		p.t.Fatalf("write: %v", err)
	}
}

func (p *peer) send(target device.ID, payload string) {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := Encode(target, []byte(payload))
	if err != nil {
		p.t.Fatalf("encode: %v", err)
	}
	if err := p.ws.Write(ctx, websocket.MessageBinary, msg); err != nil {
		p.t.Fatalf("send: %v", err)
	}
}

// recv returns the sender and body of the next routed message.
func (p *peer) recv(timeout time.Duration) (device.ID, string, error) {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, b, err := p.ws.Read(ctx)
	if err != nil {
		return "", "", err
	}
	from, payload, err := Decode(b)
	if err != nil {
		return "", "", err
	}
	return from, string(payload), nil
}

// --- cases -------------------------------------------------------------------

func TestEncodeRejectsInvalidTarget(t *testing.T) {
	if _, err := Encode("abc", []byte("payload")); err != device.ErrBadID {
		t.Fatalf("error = %v, want %v", err, device.ErrBadID)
	}
}

func TestClientToHostRouting(t *testing.T) {
	h := newHarness(t)
	host, bye := h.connect("group-a", handshake.RoleRuntime)
	if bye != nil {
		t.Fatalf("host refused: %s", bye.Message)
	}
	phone, bye := h.connect("group-a", handshake.RoleClient)
	if bye != nil {
		t.Fatalf("phone refused: %s", bye.Message)
	}

	phone.send(host.id, "run the tests")

	from, body, err := host.recv(3 * time.Second)
	if err != nil {
		t.Fatalf("host received nothing: %v", err)
	}
	if body != "run the tests" {
		t.Errorf("body = %q", body)
	}
	if from != phone.id {
		t.Errorf("sender = %s, want the phone's id %s", from, phone.id)
	}
}

func TestHostToClientRouting(t *testing.T) {
	h := newHarness(t)
	host, _ := h.connect("group-a", handshake.RoleRuntime)
	phone, _ := h.connect("group-a", handshake.RoleClient)

	host.send(phone.id, "turn finished")

	from, body, err := phone.recv(3 * time.Second)
	if err != nil {
		t.Fatalf("phone received nothing: %v", err)
	}
	if body != "turn finished" || from != host.id {
		t.Errorf("got %q from %s", body, from)
	}
}

func TestOneClientCanTargetMultipleHosts(t *testing.T) {
	h := newHarness(t)
	hostA, _ := h.connect("group-a", handshake.RoleRuntime)
	phone, _ := h.connect("group-a", handshake.RoleClient)
	hostB, _ := h.connect("group-b", handshake.RoleRuntime)

	phone.send(hostA.id, "for host a")
	phone.send(hostB.id, "for host b")

	if from, body, err := hostA.recv(3 * time.Second); err != nil || body != "for host a" || from != phone.id {
		t.Fatalf("host A got %q from %s: %v", body, from, err)
	}
	if from, body, err := hostB.recv(3 * time.Second); err != nil || body != "for host b" || from != phone.id {
		t.Fatalf("host B got %q from %s: %v", body, from, err)
	}
}

// Two runtimes in one group are addressed independently, and neither evicts
// the other. Multi-host rests on this.
func TestSecondRuntimeDoesNotEvictTheFirst(t *testing.T) {
	h := newHarness(t)
	hostA, _ := h.connect("group-a", handshake.RoleRuntime)
	hostB, _ := h.connect("group-a", handshake.RoleRuntime)
	phone, _ := h.connect("group-a", handshake.RoleClient)

	phone.send(hostA.id, "exact a")
	phone.send(hostB.id, "exact b")
	if from, body, err := hostA.recv(3 * time.Second); err != nil || body != "exact a" || from != phone.id {
		t.Fatalf("host A got %q from %s: %v", body, from, err)
	}
	if from, body, err := hostB.recv(3 * time.Second); err != nil || body != "exact b" || from != phone.id {
		t.Fatalf("host B got %q from %s: %v", body, from, err)
	}
}

func TestUnknownTargetIsDropped(t *testing.T) {
	h := newHarness(t)
	host, _ := h.connect("group-a", handshake.RoleRuntime)
	phoneA, _ := h.connect("group-a", handshake.RoleClient)

	unknown := make([]byte, device.IDLen)
	if _, err := rand.Read(unknown); err != nil {
		t.Fatal(err)
	}
	phoneA.send(device.IDFromBytes(unknown), "nowhere")

	if _, body, err := host.recv(300 * time.Millisecond); err == nil {
		t.Fatalf("an untargeted host received traffic: %q", body)
	}
}

func TestUnsignedHelloIsRejected(t *testing.T) {
	h := newHarness(t)
	_, bye := h.connect("group-a", handshake.RoleClient, func(hello *handshake.Hello) {
		hello.ChallengeSig = nil
	})
	if bye == nil {
		t.Fatal("an unsigned hello was accepted")
	}
}

func TestMissingGroupIsRejected(t *testing.T) {
	h := newHarness(t)
	_, bye := h.connect("group-a", handshake.RoleClient, func(hello *handshake.Hello) {
		hello.GroupID = ""
	})
	if bye == nil {
		t.Fatal("a hello without its compatibility group was accepted")
	}
}

func TestMismatchedIdAndKeyIsRejected(t *testing.T) {
	h := newHarness(t)
	other, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Someone else's id beside our own key: self-certifying ids only work if
	// something checks the pairing.
	_, bye := h.connect("group-a", handshake.RoleClient, func(hello *handshake.Hello) {
		hello.DeviceID = string(device.IDFor(other))
	})
	if bye == nil {
		t.Fatal("a device id that does not match its key was accepted")
	}
}

func TestLocalTokenIsRejected(t *testing.T) {
	h := newHarness(t)
	_, bye := h.connect("group-a", handshake.RoleClient, func(hello *handshake.Hello) {
		hello.LocalToken = "loopback-token"
	})
	if bye == nil {
		t.Fatal("the relay accepted a loopback token, which is meaningless off-box")
	}
}

func TestReconnectReplacesTheOldConnection(t *testing.T) {
	h := newHarness(t)
	host, _ := h.connect("group-a", handshake.RoleRuntime)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	first, _ := h.connectAs(pub, priv, "group-a", handshake.RoleClient)
	second, bye := h.connectAs(pub, priv, "group-a", handshake.RoleClient)
	if bye != nil {
		t.Fatalf("reconnect refused: %s", bye.Message)
	}

	// The stale socket must not keep the route: a phone that changed networks
	// would otherwise have its replies delivered to a connection nobody reads.
	host.send(second.id, "hello again")
	if _, body, err := second.recv(3 * time.Second); err != nil || body != "hello again" {
		t.Fatalf("new connection did not receive: %q %v", body, err)
	}
	if _, _, err := first.recv(500 * time.Millisecond); err == nil {
		t.Error("the replaced connection is still receiving traffic")
	}
}

func TestShortEnvelopeDoesNotKillTheConnection(t *testing.T) {
	h := newHarness(t)
	host, _ := h.connect("group-a", handshake.RoleRuntime)
	phone, _ := h.connect("group-a", handshake.RoleClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := phone.ws.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A malformed message is one bad message, not a reason to drop a device.
	phone.send(host.id, "still here")
	if _, body, err := host.recv(3 * time.Second); err != nil || body != "still here" {
		t.Fatalf("connection unusable after a malformed message: %q %v", body, err)
	}
}

func TestPayloadIsForwardedVerbatim(t *testing.T) {
	h := newHarness(t)
	host, _ := h.connect("group-a", handshake.RoleRuntime)
	phone, _ := h.connect("group-a", handshake.RoleClient)

	// Not valid protobuf, not valid anything. The relay must not care — this is
	// what "never parses a payload" has to mean once payloads are encrypted.
	raw := []byte{0xff, 0x00, 0xde, 0xad, 0xbe, 0xef}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := Encode(host.id, raw)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := phone.ws.Write(ctx, websocket.MessageBinary, msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, body, err := host.recv(3 * time.Second)
	if err != nil {
		t.Fatalf("host received nothing: %v", err)
	}
	if hex.EncodeToString([]byte(body)) != hex.EncodeToString(raw) {
		t.Errorf("payload altered in transit: %x", body)
	}
}

// A phone holds one socket per paired host, all under its one id. Neither
// evicts the other, and each host's reply goes down the socket that spoke to it.
func TestOneDeviceHoldsASocketPerHost(t *testing.T) {
	h := newHarness(t)
	hostA, _ := h.connect("group-a", handshake.RoleRuntime)
	hostB, _ := h.connect("group-a", handshake.RoleRuntime)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	toA, _ := h.connectAs(pub, priv, "group-a", handshake.RoleClient)
	toB, bye := h.connectAs(pub, priv, "group-a", handshake.RoleClient)
	if bye != nil {
		t.Fatalf("second socket refused: %s", bye.Message)
	}

	toA.send(hostA.id, "hello a")
	toB.send(hostB.id, "hello b")
	if _, body, err := hostA.recv(3 * time.Second); err != nil || body != "hello a" {
		t.Fatalf("host A got %q: %v", body, err)
	}
	if _, body, err := hostB.recv(3 * time.Second); err != nil || body != "hello b" {
		t.Fatalf("host B got %q: %v", body, err)
	}

	hostA.send(toA.id, "reply a")
	hostB.send(toB.id, "reply b")
	if from, body, err := toA.recv(3 * time.Second); err != nil || body != "reply a" || from != hostA.id {
		t.Fatalf("socket A got %q from %s: %v", body, from, err)
	}
	if from, body, err := toB.recv(3 * time.Second); err != nil || body != "reply b" || from != hostB.id {
		t.Fatalf("socket B got %q from %s: %v", body, from, err)
	}
}

// Past the per-device bound the oldest socket goes, not the newest.
func TestOldestSocketGoesPastTheBound(t *testing.T) {
	h := newHarness(t)
	host, _ := h.connect("group-a", handshake.RoleRuntime)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var socks []*peer
	for range socketsPerDevice + 1 {
		p, bye := h.connectAs(pub, priv, "group-a", handshake.RoleClient)
		if bye != nil {
			t.Fatalf("refused: %s", bye.Message)
		}
		socks = append(socks, p)
	}
	if _, _, err := socks[0].recv(2 * time.Second); err == nil {
		t.Fatal("the oldest socket is still open")
	}
	newest := socks[len(socks)-1]
	host.send(newest.id, "still here")
	if _, body, err := newest.recv(3 * time.Second); err != nil || body != "still here" {
		t.Fatalf("newest socket got %q: %v", body, err)
	}
}

// flush round-trips a control request and fails on any frame ahead of its
// reply: a socket's frames are handled in order, so what earlier ones caused
// is already queued.
func (p *peer) flush() {
	p.t.Helper()
	p.send(ControlID, `{"jsonrpc":"2.0","id":"flush","method":"relay.flush"}`)
	from, body, err := p.recv(3 * time.Second)
	if err != nil {
		p.t.Fatalf("no flush reply: %v", err)
	}
	if msg, err := jsonrpc.Decode([]byte(body)); from != ControlID || err != nil || string(msg.ID) != `"flush"` {
		p.t.Fatalf("got %s from %s ahead of the flush reply", body, from)
	}
}

// presence reads the next frame as a presence notice from the relay.
func (p *peer) presence(timeout time.Duration) (string, Presence, error) {
	p.t.Helper()
	from, body, err := p.recv(timeout)
	if err != nil {
		return "", Presence{}, err
	}
	if from != ControlID {
		return "", Presence{}, fmt.Errorf("frame from %s, want the relay", from)
	}
	msg, err := jsonrpc.Decode([]byte(body))
	if err != nil {
		return "", Presence{}, err
	}
	var params Presence
	err = jsonrpc.Into(msg.Params, &params)
	return msg.Method, params, err
}

// The host hears a phone it was talking to go and come back, as it happens,
// rather than on a lease running out.
func TestAPeerHearsTheOtherSideGoAndComeBack(t *testing.T) {
	h := newHarness(t)
	host, _ := h.connect("group-a", handshake.RoleRuntime)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	phone, _ := h.connectAs(pub, priv, "group-a", handshake.RoleClient)
	phone.send(host.id, "hello")
	if _, _, err := host.recv(3 * time.Second); err != nil {
		t.Fatal(err)
	}

	phone.ws.Close(websocket.StatusNormalClosure, "")
	method, p, err := host.presence(3 * time.Second)
	if err != nil || method != PresenceGone || p.Device != phone.id {
		t.Fatalf("after the phone left: %q %+v %v, want %s for the phone", method, p, err, PresenceGone)
	}

	_, _ = h.connectAs(pub, priv, "group-a", handshake.RoleClient)
	method, p, err = host.presence(3 * time.Second)
	if err != nil || method != PresenceBack || p.Device != phone.id {
		t.Fatalf("after the phone returned: %q %+v %v, want %s for the phone", method, p, err, PresenceBack)
	}
}

// One of a device's sockets closing is a network change, not a departure.
func TestClosingOneOfTwoSocketsIsNotADeparture(t *testing.T) {
	h := newHarness(t)
	host, _ := h.connect("group-a", handshake.RoleRuntime)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := h.connectAs(pub, priv, "group-a", handshake.RoleClient)
	_, _ = h.connectAs(pub, priv, "group-a", handshake.RoleClient)
	first.send(host.id, "hello")
	if _, _, err := host.recv(3 * time.Second); err != nil {
		t.Fatal(err)
	}

	first.ws.Close(websocket.StatusNormalClosure, "")
	if method, _, err := host.presence(500 * time.Millisecond); err == nil {
		t.Errorf("host was told %q while the phone still holds a socket", method)
	}
}

// A device that never spoke to anyone leaves without a notice.
func TestAStrangerLeavesSilently(t *testing.T) {
	h := newHarness(t)
	host, _ := h.connect("group-a", handshake.RoleRuntime)
	phone, _ := h.connect("group-a", handshake.RoleClient)

	phone.ws.Close(websocket.StatusNormalClosure, "")
	h.gone(phone.id)
	h.srv.mu.RLock()
	_, retained := h.srv.away[phone.id]
	h.srv.mu.RUnlock()
	if retained {
		t.Error("a device with no peers left retained presence state")
	}
	if method, _, err := host.presence(500 * time.Millisecond); err == nil {
		t.Errorf("host was told %q about a device it never heard from", method)
	}
}

// A phone that opens after its host has gone learns it on its first frame,
// not from a timeout.
func TestALateJoinerIsToldTheTargetIsGone(t *testing.T) {
	h := newHarness(t)
	host, _ := talking(t, h)
	host.ws.Close(websocket.StatusNormalClosure, "")
	h.gone(host.id)
	h.srv.graceOver(host.id)

	phone, _ := h.connect("group-a", handshake.RoleClient)
	phone.send(host.id, "anyone there?")
	method, p, err := phone.presence(3 * time.Second)
	if err != nil || method != PresenceGone || p.Device != host.id {
		t.Fatalf("got %q %+v %v, want %s for the host", method, p, err, PresenceGone)
	}

	// Said once, not once per frame: the next notice is the host coming back,
	// which the phone hears as the peers the host left behind do.
	phone.send(host.id, "still there?")
	phone.flush()
	_, _ = h.connectAs(host.pub, host.priv, "group-a", handshake.RoleRuntime)
	method, p, err = phone.presence(3 * time.Second)
	if err != nil || method != PresenceBack || p.Device != host.id {
		t.Fatalf("next notice: %q %+v %v, want %s — not %s again", method, p, err, PresenceBack, PresenceGone)
	}
}

// The relay does not tell a device outside the pairing group whether an id
// is online.
func TestAStrangerIsNotToldWhoIsGone(t *testing.T) {
	h := newHarness(t)
	host, _ := talking(t, h)
	host.ws.Close(websocket.StatusNormalClosure, "")
	h.gone(host.id)
	h.srv.graceOver(host.id)

	stranger, _ := h.connect("group-b", handshake.RoleClient)
	stranger.send(host.id, "anyone there?")
	stranger.flush()
}

// talking returns a host and a phone that have exchanged a frame, so the relay
// counts each as the other's peer, plus the host's keys to reconnect with.
func talking(t *testing.T, h *harness) (host, phone *peer) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host, _ = h.connectAs(pub, priv, "group-a", handshake.RoleRuntime)
	phone, _ = h.connect("group-a", handshake.RoleClient)
	phone.send(host.id, "hello")
	if _, _, err := host.recv(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	return host, phone
}

// A host that restarts inside the grace is only announced back: its phones
// start a fresh channel and never show it offline.
func TestAHostRestartInsideTheGraceIsOnlyBack(t *testing.T) {
	h := newHarness(t)
	host, phone := talking(t, h)

	host.ws.Close(websocket.StatusNormalClosure, "")
	h.gone(host.id)
	_, _ = h.connectAs(host.pub, host.priv, "group-a", handshake.RoleRuntime)

	method, p, err := phone.presence(3 * time.Second)
	if err != nil || method != PresenceBack || p.Device != host.id {
		t.Fatalf("got %q %+v %v, want only %s", method, p, err, PresenceBack)
	}
}

// A host that closed cleanly and stays away is announced once the grace is
// over, as closed.
func TestAHostThatStaysAwayIsAnnouncedAfterTheGrace(t *testing.T) {
	h := newHarness(t)
	host, phone := talking(t, h)

	host.ws.Close(websocket.StatusNormalClosure, "")
	start := time.Now()
	method, p, err := phone.presence(hostGrace + 3*time.Second)
	if err != nil || method != PresenceGone || p.Device != host.id || p.Reason != ReasonClosed {
		t.Fatalf("got %q %+v %v, want %s closed", method, p, err, PresenceGone)
	}
	if waited := time.Since(start); waited < hostGrace-200*time.Millisecond {
		t.Errorf("announced after %v, before the %v grace", waited, hostGrace)
	}
}

// A host whose relay link redialed before the relay noticed the old socket:
// the new socket replaces the old, frames go to the new one, and its phones
// hear it is back, because the host lost their channels with the old one.
func TestASecondHostSocketReplacesTheFirst(t *testing.T) {
	h := newHarness(t)
	host, phone := talking(t, h)

	second, _ := h.connectAs(host.pub, host.priv, "group-a", handshake.RoleRuntime)
	method, p, err := phone.presence(3 * time.Second)
	if err != nil || method != PresenceBack || p.Device != host.id {
		t.Fatalf("got %q %+v %v, want %s", method, p, err, PresenceBack)
	}

	phone.send(host.id, "to the new socket")
	if _, body, err := second.recv(3 * time.Second); err != nil || body != "to the new socket" {
		t.Fatalf("the new socket got %q %v", body, err)
	}
	if _, _, err := host.recv(500 * time.Millisecond); err == nil {
		t.Error("the replaced socket still receives frames")
	}
}

// The largest message, sealed and enveloped, still fits through the relay.
func TestALargestFrameCrossesTheRelay(t *testing.T) {
	h := newHarness(t)
	host, _ := h.connect("group-a", handshake.RoleRuntime)
	phone, _ := h.connect("group-a", handshake.RoleClient)
	phone.ws.SetReadLimit(MaxFrameBytes)

	body := make([]byte, MaxFrameBytes-headerLen)
	rand.Read(body)
	host.send(phone.id, string(body))
	if _, got, err := phone.recv(10 * time.Second); err != nil || got != string(body) {
		t.Fatalf("phone got %d bytes (%v), want %d", len(got), err, len(body))
	}
}

// A phone that stops reading is closed once its queue fills, and never holds
// up the host's frames to anyone else.
func TestAStalledReaderDoesNotStallOthers(t *testing.T) {
	h := newHarnessWith(t, Config{}, func(s *Server) { s.outboxBytes = 1 << 20 })
	host, _ := h.connect("group-a", handshake.RoleRuntime)
	stalled, _ := h.connect("group-a", handshake.RoleClient)
	phone, _ := h.connect("group-a", handshake.RoleClient)
	stalled.ws.SetReadLimit(1 << 20)

	// Past the queue and both kernels' socket buffers.
	frame := strings.Repeat("x", 64<<10)
	for range 512 {
		host.send(stalled.id, frame)
	}
	host.send(phone.id, "meanwhile")
	if _, body, err := phone.recv(3 * time.Second); err != nil || body != "meanwhile" {
		t.Fatalf("the other phone got %q %v while one stalled", body, err)
	}
	for {
		_, _, err := stalled.recv(5 * time.Second)
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("the stalled phone was never closed")
		}
		if err != nil {
			break
		}
	}
}

// A frame to an id that is not here leaves nothing behind: the table must not
// grow with whatever ids a stranger invents.
func TestAFrameToAnAbsentIDLeavesNoRoute(t *testing.T) {
	h := newHarness(t)
	sender, _ := h.connect("group-a", handshake.RoleClient)
	for i := range 50 {
		sender.send(device.ID(fmt.Sprintf("%032x", i)), "anyone?")
	}
	sender.flush()
	h.srv.mu.RLock()
	defer h.srv.mu.RUnlock()
	if n := len(h.srv.routes); n != 0 {
		t.Fatalf("relay remembers %d routes to absent ids, want 0", n)
	}
}

// One socket is remembered talking to at most routesPerSocket targets.
func TestRoutesPerSocketAreCapped(t *testing.T) {
	h := newHarness(t)
	sender, _ := h.connect("group-a", handshake.RoleClient)
	for range routesPerSocket + 4 {
		target, _ := h.connect("group-a", handshake.RoleClient)
		sender.send(target.id, "hello")
		if _, _, err := target.recv(3 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	h.srv.mu.RLock()
	defer h.srv.mu.RUnlock()
	if n := len(h.srv.routes); n != routesPerSocket {
		t.Fatalf("relay remembers %d routes for one socket, want %d", n, routesPerSocket)
	}
}

// Sending a frame to a device does not sign the sender up for its presence:
// only a peer in the device's group hears it go.
func TestAStrangerOutsideTheGroupHearsNoPresence(t *testing.T) {
	h := newHarness(t)
	phone, _ := h.connect("group-a", handshake.RoleClient)
	stranger, _ := h.connect("group-b", handshake.RoleClient)
	stranger.send(phone.id, "watching you")
	if _, _, err := phone.recv(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	phone.ws.Close(websocket.StatusNormalClosure, "")
	h.gone(phone.id)
	if method, p, err := stranger.presence(500 * time.Millisecond); err == nil {
		t.Fatalf("a stranger outside the group was told %q about %s", method, p.Device)
	}
}
