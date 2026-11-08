package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/internal/redial"
	"github.com/repogoapp/host/tunnelwire"
)

// nonceLen is the challenge nonce's size; the gateway makes 32 bytes.
const nonceLen = 32

// pingEvery keeps the socket alive through idle NATs and notices a dead gateway.
const pingEvery = 30 * time.Second

// writeTimeout bounds one frame to the gateway; a socket that can't take one
// in this long is dead, and the write closes it.
const writeTimeout = 60 * time.Second

// slots is a fixed number of places, tunnelwire's limits. One past them is
// refused at once, so the socket's reader never waits for room; the Service
// holds them, so work an old socket left running still counts after a reconnect.
type slots chan struct{}

func (s slots) take() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s slots) give() { <-s }

func (s *Service) dialLoop(ctx context.Context) {
	redial.Run(ctx, func(ctx context.Context) error {
		defer s.setConnected(false)
		return s.connect(ctx)
	}, func(err error, wait time.Duration) {
		s.cfg.Log.Warn("tunnel: gateway disconnected", "err", err, "retry_in", wait)
	})
}

// link is one gateway socket: its sender and the WebSockets it carries.
type link struct {
	s    *Service
	ctx  context.Context
	conn *websocket.Conn

	sendMu sync.Mutex
	mu     sync.Mutex
	ws     map[string]*wsSession

	reqMu    sync.Mutex
	requests map[string]context.CancelFunc // HTTP and plugin requests in flight, by session
}

func (l *link) send(m tunnelwire.Message) error {
	b, err := tunnelwire.Encode(m)
	if err != nil {
		return err
	}
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	ctx, cancel := context.WithTimeout(l.ctx, writeTimeout)
	defer cancel()
	return l.conn.Write(ctx, websocket.MessageBinary, b)
}

func (l *link) recv() (tunnelwire.Message, error) {
	_, b, err := l.conn.Read(l.ctx)
	if err != nil {
		return nil, err
	}
	return tunnelwire.Decode(b)
}

// gatewayURL is the connect address and the name the gateway must sign for.
func (s *Service) gatewayURL() (url, gatewayID string) {
	gatewayID = s.cfg.Gateway
	if host, _, err := net.SplitHostPort(s.cfg.Gateway); err == nil {
		gatewayID = host
	}
	scheme := "wss://"
	if s.cfg.Plaintext {
		scheme = "ws://"
	}
	return scheme + s.cfg.Gateway + tunnelwire.ConnectPath, gatewayID
}

func (s *Service) connect(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	url, gatewayID := s.gatewayURL()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(tunnelwire.MaxMessage)
	l := &link{s: s, ctx: ctx, conn: conn, ws: map[string]*wsSession{}, requests: map[string]context.CancelFunc{}}
	defer l.closeAll()

	if err := l.prove(gatewayID); err != nil {
		return err
	}
	s.mu.Lock()
	s.link = l
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.link = nil
		s.mu.Unlock()
	}()
	s.setConnected(true)
	s.cfg.Log.Info("tunnel: gateway connected", "gateway", s.cfg.Gateway, "host", s.cfg.Identity.ID)
	go l.keepAlive(cancel)

	for {
		m, err := l.recv()
		if err != nil {
			return err
		}
		switch m := m.(type) {
		case *tunnelwire.Request:
			l.admit(ctx, m.SessionID, s.httpSlots, func(ctx context.Context) { l.serveHTTP(ctx, m) })
		case *tunnelwire.PluginRequest:
			l.admit(ctx, m.SessionID, s.pluginSlots, func(ctx context.Context) { l.servePlugin(ctx, m) })
		case *tunnelwire.HTTPCancel:
			l.cancel(m.SessionID)
		case *tunnelwire.WSOpen:
			l.admitWS(ctx, m)
		case *tunnelwire.WSFrame:
			l.frameWS(m)
		case *tunnelwire.WSClose:
			l.closeWS(m)
		}
	}
}

// admit runs one request under its own context, registered before it starts
// so an HTTPCancel right behind it finds it. Its slot is given back only once
// serve returns, which for a plugin is once Bun has answered.
func (l *link) admit(ctx context.Context, id string, pool slots, serve func(context.Context)) {
	if id == "" {
		return
	}
	if !pool.take() {
		go l.sendError(id, http.StatusServiceUnavailable, "This computer is answering as many requests as it can. Try again in a moment.", "Computer busy")
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	l.reqMu.Lock()
	if _, taken := l.requests[id]; taken {
		l.reqMu.Unlock()
		cancel()
		pool.give()
		return
	}
	l.requests[id] = cancel
	l.reqMu.Unlock()
	go func() {
		defer pool.give()
		defer func() {
			l.reqMu.Lock()
			delete(l.requests, id)
			l.reqMu.Unlock()
			cancel()
		}()
		serve(ctx)
	}()
}

// cancel ends a request the gateway gave up on; one already done is a no-op.
func (l *link) cancel(id string) {
	l.reqMu.Lock()
	cancel := l.requests[id]
	l.reqMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// admitWS opens a visitor's WebSocket while there is room for one.
func (l *link) admitWS(ctx context.Context, open *tunnelwire.WSOpen) {
	if open.SessionID == "" {
		return
	}
	if !l.s.wsSlots.take() {
		go l.send(&tunnelwire.WSOpenAck{SessionID: open.SessionID, CloseCode: int(websocket.StatusTryAgainLater), CloseReason: "computer busy"})
		return
	}
	go func() {
		defer l.s.wsSlots.give()
		l.openWS(ctx, open)
	}()
}

// prove answers the gateway's challenge with the device key, so only this
// host can take the traffic of the tunnels it opened.
func (l *link) prove(gatewayID string) error {
	id := l.s.cfg.Identity
	if err := l.send(&tunnelwire.Hello{PublicKey: id.Public, CancelsHTTP: true}); err != nil {
		return err
	}
	m, err := l.recv()
	if err != nil {
		return err
	}
	c, ok := m.(*tunnelwire.Challenge)
	if !ok {
		return fmt.Errorf("gateway answered hello with kind %d, not a challenge", m.Kind())
	}
	// A challenge naming another gateway would let that one replay our proof.
	if c.GatewayID != gatewayID {
		return fmt.Errorf("gateway challenge names %q, not %q", c.GatewayID, gatewayID)
	}
	// A nonce of any other size would let the gateway choose how much of
	// the signed message is its own.
	if len(c.Nonce) != nonceLen {
		return fmt.Errorf("gateway challenge nonce is %d bytes, not %d", len(c.Nonce), nonceLen)
	}
	if err := l.send(&tunnelwire.Proof{Signature: id.Sign(tunnelwire.ProofMessage(c.Nonce, c.GatewayID, c.Time))}); err != nil {
		return err
	}
	m, err = l.recv()
	if err != nil {
		return err
	}
	if _, ok := m.(*tunnelwire.Ready); !ok {
		return errors.New("gateway did not accept the proof")
	}
	return nil
}

// keepAlive ends the link when the gateway stops answering pings.
func (l *link) keepAlive(cancel context.CancelFunc) {
	tick := time.NewTicker(pingEvery)
	defer tick.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-tick.C:
			ctx, done := context.WithTimeout(l.ctx, 10*time.Second)
			err := l.conn.Ping(ctx)
			done()
			if err != nil {
				cancel()
				return
			}
		}
	}
}
