// Package wsconn is the WebSocket connection the relay, the loopback server,
// the relay link, and the Go client share: a serialized writer, both sides of
// the handshake, and a ping loop for peers that vanish without a close frame.
package wsconn

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/jsonrpc"
)

const (
	// WriteTimeout bounds every write. A peer that stops reading must not hold
	// the sender's mutex forever.
	WriteTimeout = 30 * time.Second

	// HandshakeTimeout bounds challenge → hello → accepted, so a peer that
	// connects and then says nothing cannot hold a slot open.
	HandshakeTimeout = 10 * time.Second
)

// Conn wraps a *websocket.Conn. coder/websocket forbids concurrent writes, so
// every write takes one mutex, which also makes a slow peer stall its own
// sender rather than grow a queue.
type Conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	// When the last frame arrived, in UnixNano: any frame proves the peer is
	// alive, so a keepalive with Idle set pings only a quiet connection.
	lastRead atomic.Int64
}

func Wrap(ws *websocket.Conn) *Conn { return &Conn{ws: ws} }

// Listen binds addr and serves mux in the background, adding an
// unauthenticated /healthz that reveals nothing but liveness.
func Listen(addr string, mux *http.ServeMux, log *slog.Logger) (*http.Server, net.Addr, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// No Read/WriteTimeout: they would kill long-lived upgraded connections.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve stopped", "addr", addr, "err", err)
		}
	}()
	return srv, ln.Addr(), nil
}

// Read returns the next frame of any type.
func (c *Conn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	typ, b, err := c.ws.Read(ctx)
	if err == nil {
		c.lastRead.Store(time.Now().UnixNano())
	}
	return typ, b, err
}

// SinceRead is how long ago the last frame arrived; a long time before any has.
func (c *Conn) SinceRead() time.Duration {
	at := c.lastRead.Load()
	if at == 0 {
		return time.Duration(1 << 62)
	}
	return time.Since(time.Unix(0, at))
}

// ReadJSON decodes the next frame as JSON-RPC, accepting either frame type: a
// client library that only sends binary is not wrong.
func (c *Conn) ReadJSON(ctx context.Context) (*jsonrpc.Message, error) {
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	return jsonrpc.Decode(b)
}

// SendJSON writes a JSON-RPC message as a text frame.
func (c *Conn) SendJSON(ctx context.Context, m *jsonrpc.Message) error {
	b, err := jsonrpc.Encode(m)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// SendBinary writes an already-framed payload, such as a relay envelope.
func (c *Conn) SendBinary(ctx context.Context, b []byte) error {
	return c.Write(ctx, websocket.MessageBinary, b)
}

// Write is the one place bytes leave: serialized and bounded by WriteTimeout.
func (c *Conn) Write(ctx context.Context, typ websocket.MessageType, b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, WriteTimeout)
	defer cancel()
	return c.ws.Write(ctx, typ, b)
}

func (c *Conn) Ping(ctx context.Context) error { return c.ws.Ping(ctx) }

// Close sends a normal close frame; CloseNow drops the socket without one.
func (c *Conn) Close() error { return c.ws.Close(websocket.StatusNormalClosure, "") }
func (c *Conn) CloseNow()    { c.ws.CloseNow() }

// Accept runs the server side of the handshake. check vets the hello given the
// bytes its ChallengeSig must cover; any failure is sent to the peer as a refusal.
func (c *Conn) Accept(ctx context.Context, serverID, version string,
	check func(h handshake.Hello, signed []byte) error) (handshake.Hello, error) {
	var helloID json.RawMessage
	h, err := func() (handshake.Hello, error) {
		ctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
		defer cancel()
		nonce := make([]byte, device.NonceLen)
		if _, err := rand.Read(nonce); err != nil {
			return handshake.Hello{}, fmt.Errorf("nonce: %w", err)
		}
		wallMS := uint64(time.Now().UnixMilli())
		challenge, err := jsonrpc.Notify(handshake.MethodChallenge, handshake.Challenge{
			Nonce: nonce, ServerID: serverID, WallMS: wallMS,
		})
		if err != nil {
			return handshake.Hello{}, err
		}
		if err := c.SendJSON(ctx, challenge); err != nil {
			return handshake.Hello{}, fmt.Errorf("send challenge: %w", err)
		}
		msg, err := c.ReadJSON(ctx)
		if err != nil {
			return handshake.Hello{}, fmt.Errorf("read hello: %w", err)
		}
		if msg.Method != handshake.MethodHello {
			return handshake.Hello{}, fmt.Errorf("expected %s, got %q", handshake.MethodHello, msg.Method)
		}
		helloID = msg.ID
		var h handshake.Hello
		if err := jsonrpc.Into(msg.Params, &h); err != nil {
			return h, fmt.Errorf("hello: %w", err)
		}
		if h.DeviceID == "" || h.Role == "" {
			return h, errors.New("missing device id or role")
		}
		signed, err := device.ChallengeMessage(nonce, serverID, wallMS)
		if err != nil {
			return h, err
		}
		if err := check(h, signed); err != nil {
			return h, err
		}
		accepted, err := jsonrpc.Result(msg.ID, handshake.Accepted{DeviceID: h.DeviceID, ServerVersion: version})
		if err != nil {
			return h, err
		}
		return h, c.SendJSON(ctx, accepted)
	}()
	if err != nil {
		c.refuse(ctx, helloID, err)
	}
	return h, err
}

// Hello runs the client side of the handshake, signing the challenge with sign
// when it is set, and returns the build the server reported.
func (c *Conn) Hello(ctx context.Context, hello handshake.Hello, sign func([]byte) []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()
	msg, err := c.ReadJSON(ctx)
	if err != nil {
		return "", fmt.Errorf("read challenge: %w", err)
	}
	if msg.Method != handshake.MethodChallenge {
		return "", fmt.Errorf("expected %s, got %q", handshake.MethodChallenge, msg.Method)
	}
	var ch handshake.Challenge
	if err := jsonrpc.Into(msg.Params, &ch); err != nil {
		return "", fmt.Errorf("challenge: %w", err)
	}
	if sign != nil {
		signed, err := device.ChallengeMessage(ch.Nonce, ch.ServerID, ch.WallMS)
		if err != nil {
			return "", fmt.Errorf("challenge: %w", err)
		}
		hello.ChallengeSig = sign(signed)
	}
	req, err := jsonrpc.Request("hello", handshake.MethodHello, hello)
	if err != nil {
		return "", err
	}
	if err := c.SendJSON(ctx, req); err != nil {
		return "", fmt.Errorf("send hello: %w", err)
	}
	reply, err := c.ReadJSON(ctx)
	if err != nil {
		return "", fmt.Errorf("read hello reply: %w", err)
	}
	if reply.Error != nil {
		return "", fmt.Errorf("refused: %w", reply.Error)
	}
	// A goodbye here is a refusal; without this it decodes as an empty Accepted.
	if reply.Method == handshake.MethodGoodbye {
		var bye handshake.Goodbye
		_ = jsonrpc.Into(reply.Params, &bye)
		return "", fmt.Errorf("refused: %s (%s)", bye.Message, bye.Reason)
	}
	var accepted handshake.Accepted
	if err := jsonrpc.Into(reply.Result, &accepted); err != nil {
		return "", fmt.Errorf("hello reply: %w", err)
	}
	if accepted.DeviceID != hello.DeviceID {
		return "", fmt.Errorf("accepted as %q, not this device", accepted.DeviceID)
	}
	return accepted.ServerVersion, nil
}

// refuse fails the hello if it arrived, since a client waiting on it reads a
// notification as success; before the hello, a goodbye is all there is to send.
func (c *Conn) refuse(ctx context.Context, helloID json.RawMessage, err error) {
	if helloID != nil {
		_ = c.SendJSON(ctx, jsonrpc.Fail(helloID, jsonrpc.CodeDenied, err.Error()))
		_ = c.Close()
		return
	}
	if bye, err := jsonrpc.Notify(handshake.MethodGoodbye,
		handshake.Goodbye{Reason: handshake.ReasonAuthFailed, Message: err.Error()}); err == nil {
		_ = c.SendJSON(ctx, bye)
	}
	_ = c.Close()
}

// Keepalive configures the ping loop.
type Keepalive struct {
	Interval time.Duration
	Timeout  time.Duration

	// Tolerance is how many consecutive misses close the connection. One
	// missed pong on a congested hotspot is a slow network, not a dead peer.
	Tolerance int

	// Confirm, when set, rechecks a miss this soon rather than a whole Interval
	// later, with ConfirmTimeout (Timeout when zero) for that ping.
	Confirm        time.Duration
	ConfirmTimeout time.Duration

	// Idle, when set, skips the ping while the peer has sent a frame within
	// Interval: a busy connection is already proven alive.
	Idle bool

	// Dead runs once, just before a peer that stopped answering is closed.
	Dead func()

	// Name prefixes log lines; Attrs identify the peer in them.
	Name  string
	Log   *slog.Logger
	Attrs []any
}

// RunKeepalive pings until ctx ends or the peer stops answering, then closes:
// an idle phone sends nothing for hours, so only an unanswered ping means death.
// It must run alongside the read loop, which is what sees the pong.
func (c *Conn) RunKeepalive(ctx context.Context, k Keepalive) {
	tolerance := max(k.Tolerance, 1)
	timer := time.NewTimer(k.Interval)
	defer timer.Stop()

	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// A frame since the last ping already answers it: wait out the rest of
		// the interval from that frame instead.
		if k.Idle && misses == 0 {
			if idle := c.SinceRead(); idle < k.Interval {
				timer.Reset(k.Interval - idle)
				continue
			}
		}
		timeout := k.Timeout
		if misses > 0 && k.ConfirmTimeout > 0 {
			timeout = k.ConfirmTimeout
		}
		pingCtx, cancel := context.WithTimeout(ctx, timeout)
		err := c.ws.Ping(pingCtx)
		cancel()
		if err == nil {
			misses = 0
			timer.Reset(k.Interval)
			continue
		}
		if ctx.Err() != nil {
			return // the connection is closing anyway
		}
		misses++
		if misses < tolerance {
			k.Log.Warn(k.Name+": ping missed", append(k.Attrs, "misses", misses, "err", err)...)
			next := k.Interval
			if k.Confirm > 0 {
				next = k.Confirm
			}
			timer.Reset(next)
			continue
		}
		// A peer that walked out of coverage never sends a close frame.
		k.Log.Warn(k.Name+": peer stopped answering pings, closing", append(k.Attrs, "err", err)...)
		if k.Dead != nil {
			k.Dead()
		}
		c.ws.CloseNow()
		return
	}
}
