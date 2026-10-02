// Package hostclient is the Go client the benches and conformance suite drive;
// Host empty talks to the socket's peer, Host set routes through the relay.
package hostclient

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/relay"
	"github.com/repogo/host/internal/securechan"
	"github.com/repogo/host/internal/wsconn"
)

const (
	dialTimeout = 15 * time.Second
	callTimeout = 30 * time.Second
)

type Config struct {
	URL      string
	Identity *device.Identity
	GroupID  string

	Role     string
	Label    string
	Platform string

	// LocalToken authenticates a same-machine caller that has no keypair. It is
	// rejected by the relay, where a signature is the only proof.
	LocalToken string

	// Host routes every message through the relay to this device, inside the
	// end-to-end channel HostPublic pins. Empty means the peer on the other
	// end of the socket IS the host.
	Host       device.ID
	HostPublic ed25519.PublicKey

	// Unsigned skips the challenge signature; a same-machine client authenticates
	// with LocalToken alone.
	Unsigned bool

	// SignAs signs with a key other than the one the hello claims, for the
	// negative case.
	SignAs *device.Identity
}

type Client struct {
	cfg  Config
	ws   *wsconn.Conn
	peer string
	sess *securechan.Session

	mu      sync.Mutex
	nextID  int
	pending map[string]chan *jsonrpc.Message
	closed  error

	// Notifications is every push the host sent: chat appends, streaming text,
	// approvals. Buffered and dropped when full — a client that stops reading
	// must not stall the connection for everything else.
	Notifications chan *jsonrpc.Message
}

// Dial connects and completes the handshake, so a returned Client is usable.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Identity == nil {
		return nil, errors.New("hostclient: identity is required")
	}
	if cfg.Role == "" {
		cfg.Role = handshake.RoleClient
	}

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	ws, _, err := websocket.Dial(dialCtx, cfg.URL, nil)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("hostclient: dial: %w", err)
	}
	ws.SetReadLimit(relay.MaxFrameBytes)

	c := &Client{
		cfg:           cfg,
		ws:            wsconn.Wrap(ws),
		pending:       map[string]chan *jsonrpc.Message{},
		Notifications: make(chan *jsonrpc.Message, 64),
	}
	if err := c.shake(ctx); err != nil {
		ws.CloseNow()
		return nil, err
	}
	if cfg.Host != "" {
		c.sess, err = securechan.Initiate(ctx, cfg.Identity, cfg.Host, cfg.HostPublic, c.writeRouted, c.readRouted)
		if err != nil {
			ws.CloseNow()
			return nil, fmt.Errorf("hostclient: secure channel: %w", err)
		}
	}
	go c.read()
	return c, nil
}

// ServerVersion is the build the peer reported at handshake.
func (c *Client) ServerVersion() string { return c.peer }

func (c *Client) Close() error { return c.ws.Close() }

// Ping round-trips a WebSocket ping. A dead connection reports nothing on its
// own — an unanswered ping is the only thing that separates it from an idle one.
func (c *Client) Ping(ctx context.Context) error { return c.ws.Ping(ctx) }

// shake runs the handshake inline, before the reader goroutine starts: these
// three messages are strictly ordered and correlating them would be ceremony.
func (c *Client) shake(ctx context.Context) error {
	sign := c.cfg.Identity.Sign
	if c.cfg.SignAs != nil {
		sign = c.cfg.SignAs.Sign
	} else if c.cfg.Unsigned {
		sign = nil
	}
	version, err := c.ws.Hello(ctx, handshake.Hello{
		DeviceID:   string(c.cfg.Identity.ID),
		PublicKey:  c.cfg.Identity.Public,
		GroupID:    c.cfg.GroupID,
		Role:       c.cfg.Role,
		Platform:   c.cfg.Platform,
		Label:      c.cfg.Label,
		LocalToken: c.cfg.LocalToken,
	}, sign)
	if err != nil {
		return fmt.Errorf("hostclient: %w", err)
	}
	c.peer = version
	return nil
}

// Call issues one request and decodes the result into `into` (nil to ignore).
// A JSON-RPC error comes back as *jsonrpc.Error.
func (c *Client) Call(ctx context.Context, method string, params, into any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	c.mu.Lock()
	if c.closed != nil {
		err := c.closed
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := strconv.Itoa(c.nextID)
	reply := make(chan *jsonrpc.Message, 1)
	c.pending[id] = reply
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	req, err := jsonrpc.Request(id, method, params)
	if err != nil {
		return err
	}
	if err := c.send(ctx, req); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf("hostclient: %s: %w", method, ctx.Err())
	case msg := <-reply:
		if msg.Error != nil {
			return msg.Error
		}
		if into == nil {
			return nil
		}
		return jsonrpc.Into(msg.Result, into)
	}
}

func (c *Client) read() {
	defer close(c.Notifications)
	for {
		var b []byte
		var err error
		if c.cfg.Host != "" {
			b, err = c.readRecord()
		} else {
			_, b, err = c.ws.Read(context.Background())
		}
		if err != nil {
			c.fail(err)
			return
		}
		msg, err := jsonrpc.Decode(b)
		if err != nil {
			continue
		}
		if msg.IsNotification() {
			select {
			case c.Notifications <- msg:
			default:
			}
			continue
		}

		c.mu.Lock()
		waiter := c.pending[string(trimQuotes(msg.ID))]
		c.mu.Unlock()
		deliver(waiter, msg)
	}
}

// fail wakes every in-flight call rather than leaving them to time out
// individually: a dropped connection is knowable immediately and thirty seconds
// of silence per call is the worst way to learn it.
func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed != nil {
		return
	}
	c.closed = fmt.Errorf("hostclient: connection closed: %w", err)
	for id, waiter := range c.pending {
		deliver(waiter, jsonrpc.Fail(nil, jsonrpc.CodeInternal, c.closed.Error()))
		delete(c.pending, id)
	}
}

// deliver hands a waiter its one reply; a duplicate, or a failure after the
// reply, finds it full and is dropped rather than block the reader.
func deliver(waiter chan *jsonrpc.Message, msg *jsonrpc.Message) {
	select {
	case waiter <- msg:
	default:
	}
}

func (c *Client) send(ctx context.Context, m *jsonrpc.Message) error {
	b, err := jsonrpc.Encode(m)
	if err != nil {
		return err
	}
	if c.cfg.Host == "" {
		return c.ws.Write(ctx, websocket.MessageText, b)
	}
	return c.sess.Seal(b, func(wire []byte) error { return c.writeRouted(ctx, wire) })
}

// writeRouted wraps a payload in the relay envelope addressed to the host.
func (c *Client) writeRouted(ctx context.Context, payload []byte) error {
	envelope, err := relay.Encode(c.cfg.Host, payload)
	if err != nil {
		return err
	}
	return c.ws.SendBinary(ctx, envelope)
}

// readRouted returns the next payload the host sent, whatever its kind.
func (c *Client) readRouted(ctx context.Context) ([]byte, error) {
	for {
		_, b, err := c.ws.Read(ctx)
		if err != nil {
			return nil, err
		}
		if _, payload, err := relay.Decode(b); err == nil && len(payload) > 0 {
			return payload, nil
		}
	}
}

// readRecord opens the next record. A reset means the host lost our
// session; like the phone, this client treats that as a dropped connection.
func (c *Client) readRecord() ([]byte, error) {
	for {
		payload, err := c.readRouted(context.Background())
		if err != nil {
			return nil, err
		}
		switch payload[0] {
		case securechan.Record:
			return c.sess.Open(payload)
		case securechan.Reset:
			return nil, securechan.ErrNoSession
		}
	}
}

// trimQuotes turns a raw JSON id back into the string we sent. Ids are opaque
// on the wire and echoed verbatim, so this is the one place that has to know
// ours are strings.
func trimQuotes(raw []byte) []byte {
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1]
	}
	return raw
}
