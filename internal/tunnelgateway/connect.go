package tunnelgateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/tunnelwire"
)

// proofTimeout bounds how long an unproven socket may hold a slot.
const proofTimeout = 10 * time.Second

// writeTimeout bounds one frame to a host; a socket that can't take one in
// this long is dead, and the write closes it.
const writeTimeout = 60 * time.Second

// hostIDFor is device.IDFor, copied so the gateway doesn't link the device
// package's dependencies; TestHostIDMatchesTheDeviceID keeps them equal.
func hostIDFor(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:16])
}

// hostSocket is a host's WebSocket, framed with tunnelwire.
type hostSocket struct {
	ctx    context.Context
	conn   *websocket.Conn
	sendMu sync.Mutex
}

func (c *hostSocket) send(m tunnelwire.Message) error {
	b, err := tunnelwire.Encode(m)
	if err != nil {
		return err
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
	defer cancel()
	return c.conn.Write(ctx, websocket.MessageBinary, b)
}

func (c *hostSocket) read(ctx context.Context) (tunnelwire.Message, error) {
	_, b, err := c.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return tunnelwire.Decode(b)
}

// serveConnect admits one host: it proves its key, then serves the tunnels
// whose rows name that key until the socket ends or a newer one replaces it.
func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.sockets.take(ip) {
		http.Error(w, "too many connections from this address", http.StatusTooManyRequests)
		return
	}
	defer s.sockets.give(ip)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(proofReadLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := &hostSocket{ctx: ctx, conn: conn}

	hostID, cancels, err := s.prove(c)
	if err != nil {
		s.Log.Warn("host refused", "err", err)
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	conn.SetReadLimit(tunnelwire.MaxMessage)
	h := NewHost(hostID, cancels, c.send, s.Log)
	evicted := s.Hub.Register(h)
	defer s.Hub.Unregister(h)
	if err := c.send(&tunnelwire.Ready{}); err != nil {
		return
	}
	s.Log.Info("host connected", "host", hostID)

	done := make(chan error, 1)
	go func() {
		for {
			m, err := c.read(ctx)
			if err != nil {
				done <- err
				return
			}
			h.Dispatch(m)
		}
	}()
	select {
	case <-evicted:
		_ = conn.Close(websocket.StatusGoingAway, "replaced by a newer connection")
	case err := <-done:
		s.Log.Info("host disconnected", "host", hostID, "err", err)
	}
}

// prove challenges the host's key and returns the id derived from it once the
// host signs the challenge for this gateway, and whether it reads HTTPCancel.
func (s *Server) prove(c *hostSocket) (string, bool, error) {
	ctx, cancel := context.WithTimeout(c.ctx, proofTimeout)
	defer cancel()
	m, err := c.read(ctx)
	if err != nil {
		return "", false, fmt.Errorf("hello: %w", err)
	}
	hello, ok := m.(*tunnelwire.Hello)
	if !ok || len(hello.PublicKey) != ed25519.PublicKeySize {
		return "", false, errors.New("first message must be a hello with an Ed25519 key")
	}
	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	challenge := &tunnelwire.Challenge{Nonce: nonce, GatewayID: s.GatewayID, Time: uint64(s.now().UnixMilli())}
	if err := c.send(challenge); err != nil {
		return "", false, err
	}
	m, err = c.read(ctx)
	if err != nil {
		return "", false, fmt.Errorf("proof: %w", err)
	}
	proof, ok := m.(*tunnelwire.Proof)
	if !ok {
		return "", false, errors.New("expected a proof")
	}
	if !ed25519.Verify(hello.PublicKey, tunnelwire.ProofMessage(nonce, challenge.GatewayID, challenge.Time), proof.Signature) {
		return "", false, errors.New("proof does not verify")
	}
	return hostIDFor(hello.PublicKey), hello.CancelsHTTP, nil
}
