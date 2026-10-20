package relay

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/handshake"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/securechan"
	"github.com/repogoapp/host/internal/wsconn"
)

const (
	// Sized for a congested hotspot, not for wifi: a tight deadline reads to the
	// user as their machine going offline.
	pingInterval = 30 * time.Second
	pingTimeout  = 30 * time.Second

	// One missed pong is a slow network, not a dead peer.
	pingTolerance = 2

	// A silent host is declared gone in about 6 to 11s, so its phones hear it
	// soon, so hosts are pinged faster than phones.
	hostPingInterval       = 5 * time.Second
	hostPingTimeout        = 3 * time.Second
	hostPingConfirm        = 1 * time.Second
	hostPingConfirmTimeout = 2 * time.Second

	// hostGrace is how long a cleanly closed host (a restart) has to return
	// before its phones hear it has gone; an unresponsive one is announced at once.
	hostGrace = 3 * time.Second

	// MaxFrameBytes is the largest frame on a relay socket: the largest message,
	// sealed, inside an envelope.
	MaxFrameBytes = jsonrpc.MaxMessageBytes + securechan.RecordOverhead + headerLen

	// outboxBytes bounds the frames queued for one socket; a reader further
	// behind than this is closed rather than let stall its senders.
	outboxBytes  = 4 * MaxFrameBytes
	outboxFrames = 1024

	// defaultBufferedBytes bounds the frames held across every socket, queued
	// or still arriving, so sockets that stall together cannot run the relay
	// out of memory.
	defaultBufferedBytes = 64 << 20

	// frameWithin bounds how long one frame may take to arrive once started:
	// the largest frame over a slow phone link, not a sender trickling bytes.
	frameWithin = 2 * time.Minute

	// socketsPerDevice bounds how many sockets one device holds at once. A
	// phone opens one per paired host, all under its one id; past this the
	// oldest, most likely a socket stranded by a network change, is closed.
	socketsPerDevice = 8

	// routesPerSocket bounds the targets one socket is remembered talking to.
	// A phone talks to its hosts and a host to its phones; past this the
	// frame still goes, it is just not remembered.
	routesPerSocket = 16

	// awayFor is how long the relay remembers who a departed device was
	// talking to, so they hear when it is back. Past it, a return is silent.
	awayFor = 24 * time.Hour

	// Presence is best effort; churn must not turn it into unbounded storage.
	maxDepartures    = 4096
	maxPresencePeers = 128
	maxUnreachable   = 8192
	maxHelloBytes    = 8 << 10

	// unreachableEvery bounds how often one sender is told the same device is
	// gone when its frames to it keep finding no route.
	unreachableEvery = 5 * time.Second
)

type Config struct {
	// Addr to bind, e.g. ":8080". Fly sets PORT.
	Addr string

	// ServerID binds a signature to this relay, so a signature made for one
	// deployment cannot be replayed against another.
	ServerID string

	// Push delivers one APNs notification for `push.send`; nil means this
	// relay answers Unavailable, never a silent OK.
	Push func(context.Context, PushRequest) error

	// VerifyPush authenticates the app instance; a self-signed identity cannot own a token.
	VerifyPush func(identity, message, attestation, assertion []byte, environment string, signedAt time.Time) error

	// ClientIPHeader names the header a trusted proxy puts the caller's
	// address in (Fly-Client-IP on Fly). Empty uses the socket's peer, which
	// behind a proxy is the proxy.
	ClientIPHeader string

	// MaxConnsPerIP bounds concurrent sockets from one address. Zero means
	// defaultConnsPerIP.
	MaxConnsPerIP int

	// MaxBufferedBytes bounds the frames held across every socket; size it
	// well under the machine's memory. Zero means defaultBufferedBytes.
	MaxBufferedBytes int64

	Log *slog.Logger
}

// Server routes opaque payloads between authenticated device ids.
type Server struct {
	cfg  Config
	log  *slog.Logger
	http *http.Server

	// Every device's sockets, oldest first, and which socket a device last
	// sent each peer from: a reply goes back down that one.
	mu     sync.RWMutex
	conns  map[device.ID][]*conn
	routes map[leg]*conn

	// Who each device that has gone was talking to, and when it went: the
	// peers told it went are told it is back.
	away          map[device.ID]departure
	presenceSweep time.Time

	// When each sender was last told a device it wrote to is gone.
	toldUnreachable map[leg]time.Time

	// Hosts that closed cleanly and are inside hostGrace: their departure is
	// announced when the timer fires, unless they attach first.
	pendingGone map[device.ID]*time.Timer

	limits        limits
	outboxBytes   int64
	maxBuffered   int64
	buffered      atomic.Int64
	frameWithin   time.Duration
	connsPerIP    int
	ipMu          sync.Mutex
	ips           map[string]int
	attempts      map[string]int
	attemptWindow time.Time

	// Pushes per token this minute, whoever asked.
	tokenMu sync.Mutex
	tokens  map[string]*tokenWindow
}

func New(cfg Config) (*Server, error) {
	if cfg.ServerID == "" {
		return nil, errors.New("relay: server id is required")
	}
	if cfg.Push != nil && cfg.VerifyPush == nil {
		return nil, errors.New("relay: push verifier is required")
	}
	perIP := cfg.MaxConnsPerIP
	if perIP <= 0 {
		perIP = defaultConnsPerIP
	}
	maxBuffered := cfg.MaxBufferedBytes
	if maxBuffered <= 0 {
		maxBuffered = defaultBufferedBytes
	}
	return &Server{
		cfg: cfg, log: cfg.Log,
		conns: map[device.ID][]*conn{}, routes: map[leg]*conn{}, away: map[device.ID]departure{},
		toldUnreachable: map[leg]time.Time{},
		pendingGone:     map[device.ID]*time.Timer{},
		attempts:        map[string]int{},
		limits:          defaultLimits, outboxBytes: outboxBytes, maxBuffered: maxBuffered, frameWithin: frameWithin, connsPerIP: perIP, ips: map[string]int{}, tokens: map[string]*tokenWindow{},
	}, nil
}

func (s *Server) Listen() (net.Addr, error) {
	srv, addr, err := wsconn.Listen(s.cfg.Addr, s.Handler(), s.log)
	if err != nil {
		return nil, fmt.Errorf("relay: listen: %w", err)
	}
	s.http = srv
	return addr, nil
}

// Handler serves the relay's routes, for a caller that owns the listener,
// such as a bench on a Unix socket.
func (s *Server) Handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	// Deliberately minimal and unauthenticated: counts, never identities. A
	// relay that can enumerate a user's devices over HTTP is a relay that knows
	// too much.
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"connections":%d,"server_id":%q}`, s.connectionCount(), s.cfg.ServerID)
	})
	return mux
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

func (s *Server) connectionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, socks := range s.conns {
		n += len(socks)
	}
	return n
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	ip, site := s.clientIP(r)
	if !s.admitIP(ip, site, time.Now()) {
		http.Error(w, "too many connections from this address", http.StatusTooManyRequests)
		return
	}
	defer s.releaseIP(ip, site)
	// Any origin: this is a public relay reached from phones, desktops and
	// browsers on networks we do not control. Origin cannot be a security
	// boundary here — the signature is.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
		// URLSession rejects compressed frames on persistent connections.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("relay: accept failed", "err", err)
		return
	}
	ws.SetReadLimit(maxHelloBytes)
	newConn(s, ws).run(r.Context())
}

type conn struct {
	srv *Server
	ws  *wsconn.Conn

	id device.ID
	// The pairing group the handshake presented: a phone presents its host's.
	group string
	role  string
	// Set by the keepalive when the peer stopped answering, so its departure
	// is announced at once and as unresponsive rather than after a grace.
	unresponsive atomic.Bool

	// Frames for the writer, and their bytes: every write after the handshake
	// goes through here, so no read loop ever waits on another socket.
	out    chan []byte
	queued atomic.Int64
	// Bytes of the frame still arriving, counted against the relay's budget.
	reading atomic.Int64
	// gone is set, under queueMu, once the writer has stopped; frames routed
	// to c after that are refused so their bytes are never counted.
	queueMu sync.Mutex
	gone    bool

	// Control requests running now, each off the read loop.
	controls chan struct{}

	// Entries in srv.routes that go through c, under srv.mu, so the cap is
	// checked without walking every route.
	routes int

	// Rate limit for push.send, per connection.
	pushMu     sync.Mutex
	pushWindow time.Time
	pushCount  int
}

func newConn(srv *Server, ws *websocket.Conn) *conn {
	return &conn{srv: srv, ws: wsconn.Wrap(ws),
		out: make(chan []byte, outboxFrames), controls: make(chan struct{}, maxControls)}
}

func (c *conn) run(ctx context.Context) {
	defer c.ws.CloseNow()

	h, err := c.ws.Accept(ctx, c.srv.cfg.ServerID, Version(), check)
	if err != nil {
		c.srv.log.Warn("relay: handshake failed", "err", err)
		return
	}
	c.id = device.ID(h.DeviceID)
	c.ws.SetReadLimit(MaxFrameBytes)
	c.group = h.GroupID
	c.role = h.Role

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	writing := make(chan struct{})
	go func() {
		defer close(writing)
		c.write(ctx)
	}()
	defer c.release(stop, writing)
	c.srv.attach(c)
	defer c.srv.detach(c)

	keepalive := wsconn.Keepalive{
		Interval: pingInterval, Timeout: pingTimeout, Tolerance: pingTolerance,
		Name: "relay", Log: c.srv.log, Attrs: []any{"device", c.id},
		Dead: func() { c.unresponsive.Store(true) },
	}
	if c.isHost() {
		keepalive.Interval, keepalive.Timeout = hostPingInterval, hostPingTimeout
		keepalive.Confirm, keepalive.ConfirmTimeout = hostPingConfirm, hostPingConfirmTimeout
		keepalive.Idle = true
	}
	go c.ws.RunKeepalive(ctx, keepalive)

	c.srv.log.Info("relay: attached", "device", c.id, "role", h.Role)

	lim := c.srv.limits
	messages := newBucket(lim.messages, lim.messageBurst)
	bytes := newBucket(lim.bytes, lim.byteBurst)
	defer c.readDone()
	for {
		typ, msg, err := c.ws.ReadHeld(ctx, c.srv.frameWithin, c.hold)
		if err != nil || messages.wait(ctx, 1) != nil || bytes.wait(ctx, float64(len(msg))) != nil {
			return
		}
		c.forward(ctx, typ, msg)
		c.readDone()
	}
}

// forward routes one binary frame; any other type is ignored.
func (c *conn) forward(ctx context.Context, typ websocket.MessageType, msg []byte) {
	if typ != websocket.MessageBinary {
		return
	}
	target, payload, err := Decode(msg)
	if err != nil {
		c.srv.log.Warn("relay: bad envelope", "device", c.id, "err", err)
		return
	}
	c.srv.route(ctx, c, target, payload)
}

// hold counts n more bytes of the frame arriving on c, refusing past the
// relay's budget after closing the socket furthest behind.
func (c *conn) hold(n int) error {
	c.reading.Add(int64(n))
	if c.srv.buffered.Add(int64(n)) <= c.srv.maxBuffered {
		return nil
	}
	c.srv.shed()
	c.srv.log.Warn("relay: buffer budget full, closing a socket mid-frame", "device", c.id)
	return errRelayFull
}

// readDone returns the frame c has finished with to the relay's budget.
func (c *conn) readDone() {
	c.srv.buffered.Add(-c.reading.Swap(0))
}

// write sends queued frames in order until ctx ends or a write fails.
func (c *conn) write(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-c.out:
			err := c.ws.SendBinary(ctx, b)
			c.unqueue(int64(len(b)))
			if err != nil {
				if ctx.Err() == nil {
					c.srv.log.Warn("relay: write failed", "to", c.id, "err", err)
				}
				c.ws.CloseNow()
				return
			}
		}
	}
}

// enqueue hands a frame to c's writer, or closes c when it is too far behind:
// waiting on it would stall whoever is sending to it. Past the relay's budget
// the frame is refused and the socket furthest behind is closed to free it.
func (c *conn) enqueue(b []byte) bool {
	n := int64(len(b))
	c.queueMu.Lock()
	if c.gone {
		c.queueMu.Unlock()
		return false
	}
	ownOK := c.queued.Add(n) <= c.srv.outboxBytes
	totalOK := c.srv.buffered.Add(n) <= c.srv.maxBuffered
	if ownOK && totalOK {
		select {
		case c.out <- b:
			c.queueMu.Unlock()
			return true
		default:
		}
	}
	c.unqueue(n)
	c.queueMu.Unlock()
	if !totalOK {
		c.srv.shed()
	}
	c.srv.log.Warn("relay: closing a socket that stopped reading", "device", c.id, "relay_full", !totalOK)
	go c.ws.CloseNow()
	return false
}

var errRelayFull = errors.New("relay: buffer budget full")

func (c *conn) unqueue(n int64) {
	c.queued.Add(-n)
	c.srv.buffered.Add(-n)
}

// release returns whatever c still had queued to the relay's budget, once its
// writer has stopped and no frame can be added.
func (c *conn) release(stop context.CancelFunc, writing <-chan struct{}) {
	stop()
	<-writing
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	c.gone = true
	c.srv.buffered.Add(-c.queued.Swap(0))
}

// shed closes the socket holding the most, the one furthest behind, so the
// budget it holds is freed for sockets that keep up.
func (s *Server) shed() {
	var worst *conn
	var most int64
	s.mu.RLock()
	for _, socks := range s.conns {
		for _, c := range socks {
			if held := c.held(); held > most {
				worst, most = c, held
			}
		}
	}
	s.mu.RUnlock()
	if worst == nil {
		return
	}
	s.log.Warn("relay: buffer budget full, closing the socket furthest behind",
		"device", worst.id, "held", most, "total", s.buffered.Load())
	go worst.ws.CloseNow()
}

func (c *conn) held() int64 { return c.queued.Load() + c.reading.Load() }

// check proves the device holds the key for the id it claims; whether it may
// call another device is the host's decision, not the relay's.
func check(h handshake.Hello, signed []byte) error {
	if h.LocalToken != "" {
		// The loopback token is meaningless off-box and accepting it would
		// invite a client to treat it as a credential that travels.
		return errors.New("local token is not accepted by the relay")
	}
	if h.GroupID == "" || len(h.GroupID) > 128 {
		return errors.New("group id must be 1 to 128 bytes")
	}
	if len(h.DeviceID) != 2*device.IDLen || len(h.Label) > 256 || len(h.Platform) > 64 {
		return errors.New("invalid hello field length")
	}
	if h.Role != handshake.RoleRuntime && h.Role != handshake.RoleClient {
		return errors.New("invalid role")
	}
	pub := ed25519.PublicKey(h.PublicKey)
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("bad public key")
	}
	// Self-certifying ids are only self-certifying if something checks: without
	// this, a caller could present someone else's id beside their own key.
	if device.IDFor(pub) != device.ID(h.DeviceID) {
		return fmt.Errorf("device id %s does not match its public key", h.DeviceID)
	}
	if !ed25519.Verify(pub, signed, h.ChallengeSig) {
		return errors.New("signature does not verify")
	}
	return nil
}

// leg is one direction of a conversation: device from, talking to peer to.
type leg struct{ from, to device.ID }

func (c *conn) isHost() bool { return c.role == handshake.RoleRuntime }

// attach adds c to its device's sockets. A host's new socket retires its old
// ones and always tells its phones it is back, since their channels died with
// the old one; a phone keeps up to socketsPerDevice, one per host.
func (s *Server) attach(c *conn) {
	s.mu.Lock()
	var back []device.ID
	first := len(s.conns[c.id]) == 0
	if first || c.isHost() {
		if d := s.away[c.id]; time.Since(d.at) < awayFor {
			back = d.peers
		}
		delete(s.away, c.id)
	}
	if t := s.pendingGone[c.id]; t != nil {
		t.Stop()
		delete(s.pendingGone, c.id)
	}
	var retired []*conn
	socks := append(s.conns[c.id], c)
	if c.isHost() {
		retired = socks[:len(socks)-1]
		for _, old := range retired {
			s.forget(old)
		}
		socks = []*conn{c}
		back = mergePeers(back, s.peersLocked(c.id))
	} else if len(socks) > socketsPerDevice {
		retired = socks[:1]
		s.forget(socks[0])
		socks = socks[1:]
	}
	s.conns[c.id] = socks
	s.mu.Unlock()
	for _, old := range retired {
		go old.ws.CloseNow()
	}
	if len(retired) > 0 && c.isHost() {
		s.log.Info("relay: replaced host socket", "device", c.id, "retired", len(retired))
	}
	s.announce(c.id, c.group, PresenceBack, "", back)
}

func (s *Server) detach(c *conn) {
	s.mu.Lock()
	peers := s.peersLocked(c.id)
	s.forget(c)
	socks := s.conns[c.id]
	found := false
	for i, other := range socks {
		if other == c {
			socks = append(socks[:i:i], socks[i+1:]...)
			found = true
			break
		}
	}
	// A socket attach already retired is not a departure: its device is here
	// on a newer one.
	if !found {
		s.mu.Unlock()
		s.log.Info("relay: detached", "device", c.id, "retired", true)
		return
	}
	reason := ReasonClosed
	if c.unresponsive.Load() {
		reason = ReasonUnresponsive
	}
	var gone []device.ID
	if len(socks) == 0 {
		delete(s.conns, c.id)
		// Its last socket: the device is gone, not moving to a new network.
		now := time.Now()
		s.prunePresence(now)
		remember := len(peers) > 0 && s.roomForDeparture(c.id)
		if remember {
			s.away[c.id] = departure{peers: peers, group: c.group, reason: reason, at: now}
		}
		if remember && c.isHost() && reason == ReasonClosed {
			// A clean close is usually a restart: give it hostGrace to return.
			id := c.id
			s.pendingGone[id] = time.AfterFunc(hostGrace, func() { s.graceOver(id) })
		} else {
			gone = peers
		}
	} else {
		s.conns[c.id] = socks
	}
	s.mu.Unlock()
	s.log.Info("relay: detached", "device", c.id, "reason", reason)
	s.announce(c.id, c.group, PresenceGone, reason, gone)
}

// graceOver announces a host that closed cleanly and did not come back.
func (s *Server) graceOver(id device.ID) {
	s.mu.Lock()
	if _, pending := s.pendingGone[id]; !pending || len(s.conns[id]) > 0 {
		s.mu.Unlock()
		return
	}
	delete(s.pendingGone, id)
	d := s.away[id]
	s.mu.Unlock()
	s.announce(id, d.group, PresenceGone, d.reason, d.peers)
}

// mergePeers is a and b without repeats.
func mergePeers(a, b []device.ID) []device.ID {
	for _, id := range b {
		if len(a) >= maxPresencePeers {
			break
		}
		if !slices.Contains(a, id) {
			a = append(a, id)
		}
	}
	return a
}

// departure is a device that has gone: who it was talking to, the pairing
// group it presented, and when.
type departure struct {
	peers  []device.ID
	group  string
	reason string
	at     time.Time
}

// peersLocked is every device id has exchanged frames with through this relay
// since each side's socket attached. Caller holds s.mu.
func (s *Server) peersLocked(id device.ID) []device.ID {
	seen := map[device.ID]bool{}
	var out []device.ID
	for r := range s.routes {
		var peer device.ID
		switch id {
		case r.from:
			peer = r.to
		case r.to:
			peer = r.from
		default:
			continue
		}
		if !seen[peer] {
			seen[peer] = true
			out = append(out, peer)
			if len(out) == maxPresencePeers {
				break
			}
		}
	}
	return out
}

// unreachable tells a sender in the departed target's group that it is gone,
// so a late joiner need not time out; the relay says nothing to anyone else.
func (s *Server) unreachable(from *conn, target device.ID) {
	now := time.Now()
	r := leg{from: from.id, to: target}
	s.mu.Lock()
	s.prunePresence(now)
	d, known := s.away[target]
	_, inGrace := s.pendingGone[target]
	// Inside a host's grace its return is expected: say nothing yet.
	if !known || now.Sub(d.at) >= awayFor || inGrace || d.group == "" || d.group != from.group || now.Sub(s.toldUnreachable[r]) < unreachableEvery {
		s.mu.Unlock()
		return
	}
	if _, held := s.toldUnreachable[r]; !held && len(s.toldUnreachable) >= maxUnreachable {
		s.mu.Unlock()
		return
	}
	s.toldUnreachable[r] = now
	// It hears the target come back, as the peers it left behind do.
	if len(d.peers) < maxPresencePeers && !slices.Contains(d.peers, from.id) {
		d.peers = append(d.peers, from.id)
		s.away[target] = d
	}
	s.mu.Unlock()
	if s.send(from, target, PresenceGone, d.reason) {
		s.log.Info("relay: presence", "device", target, "method", PresenceGone, "to", from.id, "why", "no route")
	}
}

// roomForDeparture evicts the oldest departure outside a grace when the cache
// is full, so churn displaces old entries instead of freezing the cache.
func (s *Server) roomForDeparture(id device.ID) bool {
	if _, held := s.away[id]; held || len(s.away) < maxDepartures {
		return true
	}
	var oldest device.ID
	var at time.Time
	for other, d := range s.away {
		if _, inGrace := s.pendingGone[other]; !inGrace && (oldest == "" || d.at.Before(at)) {
			oldest, at = other, d.at
		}
	}
	if oldest == "" {
		return false
	}
	delete(s.away, oldest)
	return true
}

// prunePresence runs at most once a minute under s.mu, keeping disconnects cheap.
func (s *Server) prunePresence(now time.Time) {
	if now.Before(s.presenceSweep) {
		return
	}
	s.presenceSweep = now.Add(time.Minute)
	for id, d := range s.away {
		if now.Sub(d.at) >= awayFor {
			delete(s.away, id)
		}
	}
	for l, at := range s.toldUnreachable {
		if now.Sub(at) >= time.Minute {
			delete(s.toldUnreachable, l)
		}
	}
}

// announce tells each peer in subject's group, over the socket it last used
// to reach subject, that subject has gone or is back. Best effort; a peer
// outside the group only ever sent frames and is told nothing.
func (s *Server) announce(subject device.ID, group, method, reason string, peers []device.ID) {
	if len(peers) == 0 {
		return
	}
	told := 0
	for _, peer := range peers {
		if to := s.socketFor(peer, subject); to != nil && to.group == group && s.send(to, subject, method, reason) {
			told++
		}
	}
	s.log.Info("relay: presence", "device", subject, "method", method, "reason", reason, "peers", len(peers), "told", told)
}

// send queues one presence notice about subject for one socket.
func (s *Server) send(to *conn, subject device.ID, method, reason string) bool {
	msg, err := jsonrpc.Notify(method, Presence{Device: subject, Reason: reason})
	if err != nil {
		return false
	}
	b, err := jsonrpc.Encode(msg)
	if err != nil {
		return false
	}
	wire, err := Encode(ControlID, b)
	if err != nil {
		return false
	}
	return to.enqueue(wire)
}

// forget drops the routes through c. Caller holds s.mu.
func (s *Server) forget(c *conn) {
	for r, via := range s.routes {
		if via == c {
			delete(s.routes, r)
		}
	}
	c.routes = 0
}

// socketFor picks target's socket for a frame from sender: the one target last
// sent sender a frame from (a phone's other sockets would discard it), else its newest.
func (s *Server) socketFor(target, sender device.ID) *conn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if via := s.routes[leg{from: target, to: sender}]; via != nil {
		return via
	}
	if socks := s.conns[target]; len(socks) > 0 {
		return socks[len(socks)-1]
	}
	return nil
}

// noteRoute records that from's frames to target leave from c. A target that
// is not here is not recorded: a stranger writing to invented ids must not
// grow this table, and presence for an absent target goes through away.
func (s *Server) noteRoute(c *conn, target device.ID) {
	r := leg{from: c.id, to: target}
	s.mu.RLock()
	settled := s.routes[r] == c || c.routes >= routesPerSocket
	s.mu.RUnlock()
	if settled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns[target]) == 0 || !slices.Contains(s.conns[c.id], c) {
		// Only while c is attached: a frame read just before detach must not
		// leave a route to a closed socket.
		return
	}
	prev := s.routes[r]
	if prev == c || c.routes >= routesPerSocket {
		return
	}
	if prev != nil {
		prev.routes--
	}
	s.routes[r] = c
	c.routes++
}

// route forwards one message. Undeliverable messages are dropped, not queued:
// durable offline delivery is a real requirement and a real design, and a
// silent in-memory buffer would look like one without being one.
func (s *Server) route(ctx context.Context, from *conn, target device.ID, payload []byte) {
	if target == ControlID {
		s.control(ctx, from, payload)
		return
	}
	s.noteRoute(from, target)
	to := s.socketFor(target, from.id)

	if to == nil || to.id == from.id {
		s.log.Debug("relay: dropped, no route",
			"from", from.id, "target", target)
		if to == nil {
			s.unreachable(from, target)
		}
		return
	}

	// Re-enveloped with the sender's id: the receiver needs to know who this
	// came from, and it must be the identity the relay verified rather than
	// anything the sender put in the payload.
	msg, err := Encode(from.id, payload)
	if err != nil {
		s.log.Error("relay: invalid authenticated sender id", "from", from.id, "err", err)
		return
	}
	to.enqueue(msg)
}
