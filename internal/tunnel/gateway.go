package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/repogo/host/internal/redial"
	gw "github.com/repogo/host/internal/tunnel/wire/gateway"
)

// nonceLen is the challenge nonce's size; the gateway makes 32 bytes.
const nonceLen = 32

// ProofMessage is what the host signs to answer the gateway's challenge; the
// gateway's hostproof.go builds the same bytes.
func ProofMessage(nonce []byte, gatewayID string, timeMS uint64) []byte {
	msg := []byte("repogo-tunnel/1|")
	msg = append(msg, nonce...)
	msg = append(msg, '|')
	msg = append(msg, gatewayID...)
	return fmt.Appendf(append(msg, '|'), "%d", timeMS)
}

func (s *Service) dialLoop(ctx context.Context) {
	redial.Run(ctx, func(ctx context.Context) error {
		defer s.setConnected(false)
		return s.connect(ctx)
	}, func(err error, wait time.Duration) {
		s.cfg.Log.Warn("tunnel: gateway disconnected", "err", err, "retry_in", wait)
	})
}

// link is one gateway stream: its sender and the WebSockets it carries.
type link struct {
	s      *Service
	stream grpc.BidiStreamingClient[gw.ClientMessage, gw.ServerMessage]

	sendMu sync.Mutex
	mu     sync.Mutex
	ws     map[string]*wsSession
}

func (l *link) send(m *gw.ClientMessage) error {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	return l.stream.Send(m)
}

func (s *Service) connect(ctx context.Context) error {
	gatewayID, _, err := net.SplitHostPort(s.cfg.Gateway)
	if err != nil {
		return fmt.Errorf("gateway address %q: %w", s.cfg.Gateway, err)
	}
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	if s.cfg.Plaintext {
		creds = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(s.cfg.Gateway,
		grpc.WithTransportCredentials(creds),
		// Matches the gateway's keepalive enforcement (MinTime 10s).
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20), grpc.MaxCallSendMsgSize(64<<20)),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := gw.NewPreviewGatewayClient(conn).Connect(ctx)
	if err != nil {
		return err
	}
	l := &link{s: s, stream: stream, ws: map[string]*wsSession{}}
	defer l.closeAll()
	s.mu.Lock()
	s.link = l
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.link = nil
		s.mu.Unlock()
	}()

	id := s.cfg.Identity
	if err := l.send(&gw.ClientMessage{Body: &gw.ClientMessage_Hello{Hello: &gw.DeviceHello{PublicKey: id.Public}}}); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	c := first.GetChallenge()
	if c == nil {
		return errors.New("gateway answered without a challenge; it predates key-signed hosts")
	}
	// A challenge naming another gateway would let that one replay our proof.
	if c.GatewayId != gatewayID {
		return fmt.Errorf("gateway challenge names %q, not %q", c.GatewayId, gatewayID)
	}
	// A nonce of any other size would let the gateway choose how much of
	// the signed message is its own.
	if len(c.Nonce) != nonceLen {
		return fmt.Errorf("gateway challenge nonce is %d bytes, not %d", len(c.Nonce), nonceLen)
	}
	proof := id.Sign(ProofMessage(c.Nonce, c.GatewayId, c.Time))
	if err := l.send(&gw.ClientMessage{Body: &gw.ClientMessage_Proof{Proof: &gw.HostProof{Signature: proof}}}); err != nil {
		return err
	}
	ready, err := stream.Recv()
	if err != nil {
		return err
	}
	if ready.GetReady() == nil {
		return errors.New("gateway did not accept the proof")
	}
	s.setConnected(true)
	s.cfg.Log.Info("tunnel: gateway connected", "gateway", s.cfg.Gateway, "host", id.ID)

	for {
		m, err := stream.Recv()
		if err != nil {
			return err
		}
		switch body := m.GetBody().(type) {
		case *gw.ServerMessage_HttpRequest:
			go l.serveHTTP(ctx, body.HttpRequest)
		case *gw.ServerMessage_WsOpen:
			go l.openWS(ctx, body.WsOpen)
		case *gw.ServerMessage_WsFrame:
			l.frameWS(ctx, body.WsFrame)
		case *gw.ServerMessage_WsClose:
			l.closeWS(body.WsClose)
		}
	}
}
