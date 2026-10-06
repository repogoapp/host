// Package hostlink is the host's outbound connection to the relay: dialling
// out is what reaches a Mac behind NAT with no inbound rule.
package hostlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/redial"
	"github.com/repogo/host/internal/relay"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/securechan"
	"github.com/repogo/host/internal/wsconn"
)

const (
	dialTimeout  = 15 * time.Second
	pingInterval = 30 * time.Second
	pingTimeout  = 30 * time.Second

	// One missed pong is a slow network, not a dead relay; the second ping
	// follows soon after, so a dead one is found in seconds.
	pingTolerance      = 2
	pingConfirm        = 5 * time.Second
	pingConfirmTimeout = 10 * time.Second

	// handshakeTTL is how long a handshake may sit between its first and last
	// message. A phone finishes in one round trip; anything older was abandoned.
	handshakeTTL = 30 * time.Second

	// strangerLogEvery bounds the log line for frames from devices this host
	// does not know, which anyone with a key pair can send.
	strangerLogEvery = time.Minute
)

type Config struct {
	// URL of the relay, e.g. wss://relay.repogo.app/ws.
	URL string

	Devices *device.Store

	// Router is the host's vocabulary — the same one the local WebSocket
	// dispatches into.
	Router *rpc.Router

	// Pairing reports whether a pairing code is live. Only then may a device
	// this host does not know open a channel, to reach an Unpaired method.
	Pairing func() bool

	// OnDisconnect runs when a device's channel ends: dropped, replaced by a
	// new handshake, or lost with the relay connection.
	OnDisconnect func(device.ID)

	Log *slog.Logger
}

type Link struct {
	cfg    Config
	log    *slog.Logger
	dialer *dialer

	mu      sync.RWMutex
	current *conn
	// attached is closed while current is set.
	attached chan struct{}
}

func New(cfg Config) (*Link, error) {
	if cfg.URL == "" {
		return nil, errors.New("hostlink: relay url is required")
	}
	if cfg.Devices == nil || cfg.Router == nil || cfg.Pairing == nil || cfg.OnDisconnect == nil {
		return nil, errors.New("hostlink: devices, router, pairing and on-disconnect are required")
	}
	return &Link{cfg: cfg, log: cfg.Log, dialer: newDialer(), attached: make(chan struct{})}, nil
}

// Connected reports whether the link is attached to the relay.
func (l *Link) Connected() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.current != nil
}

// Attached is closed once the link is attached to the relay, until it drops.
func (l *Link) Attached() <-chan struct{} {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.attached
}

// Run dials and redials until the context is cancelled.
func (l *Link) Run(ctx context.Context) {
	redial.Run(ctx, l.session, func(err error, wait time.Duration) {
		if err != nil {
			l.log.Warn("hostlink: disconnected", "err", err, "retry_in", wait)
		}
	})
}

func (l *Link) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	// No WebSocket compression: records are ciphertext, and securechan
	// compresses the plaintext before sealing it.
	ws, _, err := websocket.Dial(dialCtx, l.cfg.URL, &websocket.DialOptions{HTTPClient: l.dialer.client})
	cancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(relay.MaxFrameBytes)

	identity := l.cfg.Devices.Identity()
	c := &conn{ws: wsconn.Wrap(ws), identity: identity,
		peers: map[device.ID]*peer{}, control: map[string]chan *jsonrpc.Message{}}
	serverVersion, err := c.ws.Hello(ctx, handshake.Hello{
		DeviceID:      string(identity.ID),
		PublicKey:     identity.Public,
		GroupID:       l.cfg.Devices.GroupID(),
		Role:          handshake.RoleRuntime,
		Platform:      runtime.GOOS,
		Label:         device.Label(),
		ClientVersion: "host",
		// No LocalToken: it means nothing off-box and the relay rejects it.
	}, identity.Sign)
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	// Set before c is published: Send uses it from other goroutines.
	ctx, stopKeepalive := context.WithCancel(ctx)
	defer stopKeepalive()
	c.base = ctx

	l.mu.Lock()
	l.current = c
	close(l.attached)
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.current == c {
			l.current = nil
			l.attached = make(chan struct{})
		}
		l.mu.Unlock()
		// Every channel rode this connection; none survives it.
		for _, id := range c.sessions() {
			l.disconnected(id)
		}
	}()
	l.log.Info("hostlink: attached to relay", "url", l.cfg.URL, "relay", serverVersion)
	// Without this the host sits on a dead socket believing it is reachable,
	// and every phone request vanishes.
	go c.ws.RunKeepalive(ctx, wsconn.Keepalive{
		Interval: pingInterval, Timeout: pingTimeout, Tolerance: pingTolerance,
		Confirm: pingConfirm, ConfirmTimeout: pingConfirmTimeout,
		Name: "hostlink", Log: l.log,
	})
	local := l.dialer.localIP()
	ticker := time.NewTicker(addressCheckEvery)
	defer ticker.Stop()
	go watchAddress(ctx, local, ticker.C, net.InterfaceAddrs, func() {
		l.log.Warn("hostlink: network changed, redialling", "local", local)
		ws.CloseNow()
	})

	for {
		typ, msg, err := c.ws.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageBinary {
			// Only the handshake is text, and it is over.
			continue
		}
		caller, payload, err := relay.Decode(msg)
		if err != nil || len(payload) == 0 {
			l.log.Warn("hostlink: bad envelope", "err", err)
			continue
		}
		if caller == relay.ControlID {
			if !l.presence(payload) {
				c.controlReply(payload)
			}
			continue
		}
		// Anyone with a key pair can address this host through the relay. The
		// relay has proved who the caller is, so a stranger is dropped here,
		// before any key exchange, allocation, or reply.
		if !l.admits(caller) {
			c.stranger(l.log, caller)
			continue
		}
		switch payload[0] {
		case securechan.Handshake:
			replaced, err := c.handshake(ctx, caller, payload)
			if err != nil {
				l.log.Warn("hostlink: handshake failed", "from", caller, "err", err)
			}
			if replaced {
				l.disconnected(caller)
			}
		case securechan.Record:
			// Opened here, in order: the nonce is a counter. Calls then run
			// concurrently, so one slow call cannot stall any other.
			ch, plain, err := c.open(caller, payload)
			if err != nil {
				l.log.Warn("hostlink: dropping session", "from", caller, "err", err)
				c.reset(ctx, caller)
				l.disconnected(caller)
				continue
			}
			msg, err := jsonrpc.Decode(plain)
			if err != nil {
				l.log.Warn("hostlink: undecodable payload", "from", caller, "err", err)
				continue
			}
			if !msg.IsRequest() {
				continue
			}
			if !ch.calls.Go(func() { l.serve(ch.ctx, c, caller, msg) }) {
				l.reply(ch.ctx, c, caller, rpc.Busy(msg))
			}
		}
	}
}

// Send delivers a notification to one device through the relay: a reply's
// envelope with no id. This is the emit.Transport the push mux falls back to,
// since the relay owns the phone's session and only it knows who is on.
func (l *Link) Send(to device.ID, method string, payload []byte) error {
	l.mu.RLock()
	c := l.current
	l.mu.RUnlock()
	if c == nil {
		return errors.New("hostlink: not attached to the relay")
	}

	b, err := jsonrpc.Encode(&jsonrpc.Message{
		JSONRPC: jsonrpc.Version,
		Method:  method,
		Params:  payload,
	})
	if err != nil {
		return err
	}
	return c.sendTo(c.base, to, b)
}

// Control asks the relay itself for something, today only push.send. The
// frame is addressed to the relay's reserved id and is the one thing on this
// socket the relay reads, so nothing private belongs in it.
func (l *Link) Control(ctx context.Context, method string, params any) (json.RawMessage, error) {
	l.mu.RLock()
	c := l.current
	l.mu.RUnlock()
	if c == nil {
		return nil, errors.New("hostlink: not attached to the relay")
	}
	id := strconv.FormatUint(c.nextControl.Add(1), 10)
	req, err := jsonrpc.Request(id, method, params)
	if err != nil {
		return nil, err
	}
	b, err := jsonrpc.Encode(req)
	if err != nil {
		return nil, err
	}
	waiter := make(chan *jsonrpc.Message, 1)
	c.mu.Lock()
	c.control[id] = waiter
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.control, id)
		c.mu.Unlock()
	}()
	if err := c.write(ctx, relay.ControlID, b); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case reply := <-waiter:
		if reply.Error != nil {
			return nil, reply.Error
		}
		return reply.Result, nil
	}
}

// serve answers one request from the relay. The caller is the id its channel
// proved; whether it was ever paired is decided here.
func (l *Link) serve(ctx context.Context, c *conn, caller device.ID, msg *jsonrpc.Message) {
	var reply *jsonrpc.Message
	if err := l.authorize(caller, msg.Method); err != nil {
		reply = jsonrpc.Fail(msg.ID, jsonrpc.CodeDenied, err.Error())
	} else {
		// Always remote: everything arriving here crossed the relay, so no
		// caller on this lane can ever reach a local-only method.
		reply = l.cfg.Router.Dispatch(ctx, rpc.Caller{Device: caller, Scope: rpc.ScopeRemote}, msg)
	}
	l.reply(ctx, c, caller, reply)
}

func (l *Link) reply(ctx context.Context, c *conn, caller device.ID, reply *jsonrpc.Message) {
	b, err := l.cfg.Router.Encode(reply)
	if err != nil {
		return
	}
	if err := c.sendTo(ctx, caller, b); err != nil && ctx.Err() == nil {
		l.log.Warn("hostlink: reply failed", "to", caller, "err", err)
	}
}

// admits reports whether a caller may open or use a channel at all: any
// device this host has paired, revoked ones included so they are told so, and
// a stranger only while a pairing code is live.
func (l *Link) admits(caller device.ID) bool {
	if _, err := l.cfg.Devices.Peer(caller); err == nil {
		return true
	}
	return l.cfg.Pairing()
}

// authorize lets an unpaired caller reach only the router's Unpaired methods,
// which the QR code guards instead.
func (l *Link) authorize(caller device.ID, method string) error {
	if l.cfg.Router.Unpaired(method) {
		return nil
	}
	peer, err := l.cfg.Devices.Peer(caller)
	if err != nil {
		l.log.Warn("hostlink: unpaired caller", "device", caller, "method", method)
		return errors.New("this device is not paired with the host")
	}
	if !peer.Active() {
		return errors.New("this device was revoked")
	}
	return nil
}

// conn is one attachment to the relay. Requests are served on their own
// goroutines, so replies genuinely race; wsconn serializes them.
type conn struct {
	ws       *wsconn.Conn
	identity *device.Identity

	// base is the attachment's lifetime; every channel's calls end with it.
	base context.Context

	mu    sync.Mutex
	peers map[device.ID]*peer

	// In-flight Control calls by request id.
	control     map[string]chan *jsonrpc.Message
	nextControl atomic.Uint64

	// Frames dropped from strangers since the last log line. Read loop only.
	strangers       int
	strangersLogged time.Time
}

// stranger counts a dropped frame and logs at most once a minute.
func (c *conn) stranger(log *slog.Logger, caller device.ID) {
	c.strangers++
	if time.Since(c.strangersLogged) < strangerLogEvery {
		return
	}
	log.Warn("hostlink: dropped frames from unpaired devices", "count", c.strangers, "last", caller)
	c.strangers, c.strangersLogged = 0, time.Now()
}

func (c *conn) controlReply(payload []byte) {
	msg, err := jsonrpc.Decode(payload)
	if err != nil || len(msg.ID) == 0 {
		return
	}
	var id string
	if json.Unmarshal(msg.ID, &id) != nil {
		return
	}
	c.mu.Lock()
	waiter := c.control[id]
	c.mu.Unlock()
	// A duplicate reply finds the waiter full and is dropped, never blocking the read loop.
	select {
	case waiter <- msg:
	default:
	}
}

// peer is one phone's channel state. A handshake in flight does not disturb
// the live session until its last message verifies.
type peer struct {
	ch *channel
	hs *securechan.Responder

	// When hs began, so an abandoned handshake can be swept.
	hsStarted time.Time
}

// channel is one established session and the calls riding it, which end with
// it: a reply can only go out on the session its request came in on.
type channel struct {
	sess   *securechan.Session
	ctx    context.Context
	cancel context.CancelFunc
	calls  rpc.Inflight
}

// handshake reports whether a finished handshake replaced a live session.
func (c *conn) handshake(ctx context.Context, from device.ID, payload []byte) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(time.Now())
	p := c.peers[from]
	if p == nil {
		p = &peer{}
		c.peers[from] = p
	}
	if p.hs == nil || len(payload) == securechan.Message1Len {
		hs, msg2, err := securechan.Respond(c.identity, from, payload)
		if err != nil {
			if p.ch == nil {
				delete(c.peers, from)
			}
			return false, err
		}
		p.hs, p.hsStarted = hs, time.Now()
		return false, c.write(ctx, from, msg2)
	}
	sess, err := p.hs.Finish(payload)
	p.hs = nil
	if err != nil {
		if p.ch == nil {
			delete(c.peers, from)
		}
		return false, err
	}
	replaced := p.ch != nil
	if replaced {
		p.ch.cancel()
	}
	ctx, cancel := context.WithCancel(c.base)
	p.ch = &channel{sess: sess, ctx: ctx, cancel: cancel, calls: rpc.NewInflight()}
	return replaced, nil
}

// sweep forgets handshakes abandoned after their first message, and the peer
// with them when it has no session. Caller holds c.mu.
func (c *conn) sweep(now time.Time) {
	for id, p := range c.peers {
		if p.hs == nil || now.Sub(p.hsStarted) < handshakeTTL {
			continue
		}
		p.hs = nil
		if p.ch == nil {
			delete(c.peers, id)
		}
	}
}

// sessions lists the devices with an established channel.
func (c *conn) sessions() []device.ID {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []device.ID
	for id, p := range c.peers {
		if p.ch != nil {
			out = append(out, id)
		}
	}
	return out
}

func (c *conn) channel(id device.ID) *channel {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.peers[id]; p != nil {
		return p.ch
	}
	return nil
}

func (c *conn) open(from device.ID, payload []byte) (*channel, []byte, error) {
	ch := c.channel(from)
	if ch == nil {
		return nil, nil, securechan.ErrNoSession
	}
	plain, err := ch.sess.Open(payload)
	return ch, plain, err
}

// reset forgets a peer and tells it so; it reconnects and handshakes again.
func (c *conn) reset(ctx context.Context, id device.ID) {
	c.mu.Lock()
	if p := c.peers[id]; p != nil && p.ch != nil {
		p.ch.cancel()
	}
	delete(c.peers, id)
	c.mu.Unlock()
	_ = c.write(ctx, id, []byte{securechan.Reset})
}

// sendTo seals a message for one device and writes it.
func (c *conn) sendTo(ctx context.Context, to device.ID, body []byte) error {
	ch := c.channel(to)
	if ch == nil {
		return securechan.ErrNoSession
	}
	return ch.sess.Seal(body, func(wire []byte) error { return c.write(ctx, to, wire) })
}

// write wraps a payload in the relay's routing header and writes it.
func (c *conn) write(ctx context.Context, to device.ID, payload []byte) error {
	envelope, err := relay.Encode(to, payload)
	if err != nil {
		return err
	}
	return c.ws.SendBinary(ctx, envelope)
}

// presence handles the relay's notice that a device has gone or is back, and
// reports whether payload was one; a gone phone's streams end now, not on lease expiry.
func (l *Link) presence(payload []byte) bool {
	msg, err := jsonrpc.Decode(payload)
	if err != nil || !msg.IsNotification() {
		return false
	}
	var p relay.Presence
	switch msg.Method {
	case relay.PresenceGone:
		if jsonrpc.Into(msg.Params, &p) == nil && p.Device != "" {
			l.log.Info("hostlink: device offline (relay)", "device", p.Device)
			l.disconnected(p.Device)
		}
	case relay.PresenceBack:
		if jsonrpc.Into(msg.Params, &p) == nil && p.Device != "" {
			l.log.Info("hostlink: device back online (relay)", "device", p.Device)
		}
	default:
		l.log.Debug("hostlink: unknown relay notice", "method", msg.Method)
	}
	return true
}

func (l *Link) disconnected(id device.ID) { l.cfg.OnDisconnect(id) }
