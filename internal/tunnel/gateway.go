package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
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
}

func (l *link) send(m tunnelwire.Message) error {
	b, err := tunnelwire.Encode(m)
	if err != nil {
		return err
	}
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	return l.conn.Write(l.ctx, websocket.MessageBinary, b)
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
	l := &link{s: s, ctx: ctx, conn: conn, ws: map[string]*wsSession{}}
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
			go l.serveHTTP(ctx, m)
		case *tunnelwire.WSOpen:
			go l.openWS(ctx, m)
		case *tunnelwire.WSFrame:
			l.frameWS(ctx, m)
		case *tunnelwire.WSClose:
			l.closeWS(m)
		}
	}
}

// prove answers the gateway's challenge with the device key, so only this
// host can take the traffic of the tunnels it opened.
func (l *link) prove(gatewayID string) error {
	id := l.s.cfg.Identity
	if err := l.send(&tunnelwire.Hello{PublicKey: id.Public}); err != nil {
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
