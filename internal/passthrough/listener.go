package passthrough

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/device"
)

// Listener is the host's end: it holds a link to the relay and yields each
// phone stream the relay announces. The streams are still the phone's TLS;
// wrap the Listener with tls.NewListener and the host's pinned certificate.
type Listener struct {
	// RelayAddr is the relay's TCP address; LinkTLS reaches its own name.
	RelayAddr string
	LinkTLS   *tls.Config
	Identity  *device.Identity
	Log       *slog.Logger
	// IdleTimeout drops a link that has sent nothing, not even a ping, for this
	// long, and dials again. Set it above the relay's ping interval.
	IdleTimeout time.Duration

	conns  chan net.Conn
	cancel context.CancelFunc
	once   sync.Once

	mu     sync.Mutex
	linked bool
}

var ErrClosed = errors.New("passthrough: listener closed")

// Listen holds the link until ctx ends or Close, dialing again when it drops.
func Listen(ctx context.Context, l *Listener) *Listener {
	ctx, l.cancel = context.WithCancel(ctx)
	l.conns = make(chan net.Conn)
	go l.run(ctx)
	return l
}

func (l *Listener) Accept() (net.Conn, error) {
	c, ok := <-l.conns
	if !ok {
		return nil, ErrClosed
	}
	return c, nil
}

func (l *Listener) Close() error {
	l.once.Do(func() { l.cancel() })
	return nil
}

func (l *Listener) Addr() net.Addr { return relayAddr(l.RelayAddr) }

// Linked reports whether the relay has accepted this host's link.
func (l *Listener) Linked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.linked
}

func (l *Listener) run(ctx context.Context) {
	defer close(l.conns)
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		err := l.hold(ctx)
		l.setLinked(false)
		if ctx.Err() != nil {
			return
		}
		l.Log.Warn("passthrough: link dropped", "err", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// hold opens the link, answers the relay's challenge and dials back for every
// notice until the link drops.
func (l *Listener) hold(ctx context.Context) error {
	conn, reader, err := l.dial(ctx, hostLine{Op: "link"})
	if err != nil {
		return err
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()
	var challenge relayLine
	if err := readLine(reader, &challenge); err != nil {
		return err
	}
	msg, err := device.ChallengeMessage(challenge.Nonce, challenge.ServerID, challenge.WallMS)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(hostLine{
		Op:        "hello",
		DeviceID:  string(l.Identity.ID),
		PublicKey: l.Identity.Public,
		Signature: l.Identity.Sign(msg),
	}); err != nil {
		return err
	}
	var answer relayLine
	if err := readLine(reader, &answer); err != nil {
		return err
	}
	if !answer.OK {
		return errors.New("passthrough: relay refused the link: " + answer.Error)
	}
	l.setLinked(true)
	for {
		if l.IdleTimeout > 0 {
			conn.SetReadDeadline(time.Now().Add(l.IdleTimeout))
		}
		var notice relayLine
		if err := readLine(reader, &notice); err != nil {
			return err
		}
		if notice.Dial != "" {
			go l.accept(ctx, notice.Dial)
		}
	}
}

func (l *Listener) accept(ctx context.Context, token string) {
	conn, reader, err := l.dial(ctx, hostLine{Op: "accept", Token: token})
	if err != nil {
		l.Log.Debug("passthrough: dial back failed", "err", err)
		return
	}
	var answer relayLine
	if err := readLine(reader, &answer); err != nil || !answer.OK {
		conn.Close()
		return
	}
	stream := &replayConn{Conn: conn, r: reader}
	select {
	case l.conns <- stream:
	case <-ctx.Done():
		conn.Close()
	}
}

func (l *Listener) dial(ctx context.Context, first hostLine) (*tls.Conn, *bufio.Reader, error) {
	d := tls.Dialer{Config: l.LinkTLS, NetDialer: &net.Dialer{Timeout: 10 * time.Second}}
	c, err := d.DialContext(ctx, "tcp", l.RelayAddr)
	if err != nil {
		return nil, nil, err
	}
	conn := c.(*tls.Conn)
	if err := json.NewEncoder(conn).Encode(first); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, bufio.NewReader(conn), nil
}

func (l *Listener) setLinked(v bool) {
	l.mu.Lock()
	l.linked = v
	l.mu.Unlock()
}

type relayAddr string

func (relayAddr) Network() string  { return "passthrough" }
func (a relayAddr) String() string { return string(a) }
