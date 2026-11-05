package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/handshake"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/release"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/wsconn"
)

const (
	pingInterval = 20 * time.Second
	pingTimeout  = 10 * time.Second
)

// Variables, so tests can shorten them.
var (
	// remoteHelloTimeout bounds a passthrough connection's challenge and
	// hello: a phone answers at once, so a stranger can't hold one open.
	remoteHelloTimeout = 5 * time.Second
	// pairTimeout bounds an unpaired device's one pair.complete, which a real
	// phone sends the moment it connects; the code's own lifetime is separate.
	pairTimeout = 15 * time.Second
)

// conn is one client connection: handshake, then a dispatch loop.
type conn struct {
	srv *Server
	ws  *wsconn.Conn

	caller rpc.Caller
	// bind is the TLS exporter of a passthrough connection; nil on loopback.
	bind []byte
	// stranger is an unpaired device admitted over passthrough while a code is
	// live: it may make one Unpaired call (pair.complete), then the socket closes.
	stranger bool
	// helloDone runs once the hello has passed or failed; nil on loopback.
	helloDone func()
}

func newConn(srv *Server, ws *websocket.Conn) *conn {
	return &conn{srv: srv, ws: wsconn.Wrap(ws)}
}

func (c *conn) run(ctx context.Context) {
	defer c.ws.CloseNow()

	// Both auth paths share one challenge: a same-machine client answers with
	// the loopback token, a paired device signs the nonce.
	helloCtx, cancelHello := ctx, context.CancelFunc(func() {})
	if c.bind != nil {
		helloCtx, cancelHello = context.WithTimeout(ctx, remoteHelloTimeout)
	}
	h, err := c.ws.Accept(helloCtx, c.srv.cfg.ServerID, release.Version, func(h handshake.Hello, signed []byte) error {
		if c.bind != nil {
			if len(h.ChallengeSig) == 0 || h.LocalToken != "" {
				return errors.New("a remote connection authenticates by a bound signature only")
			}
			signed = device.BindTLS(signed, c.bind)
			if c.srv.admitsStranger(h, signed) {
				c.stranger = true
				c.caller = rpc.Caller{Device: device.ID(h.DeviceID), Scope: rpc.ScopeRemote}
				return nil
			}
		}
		scope, err := c.srv.authenticate(&h, signed)
		c.caller = rpc.Caller{Device: device.ID(h.DeviceID), Scope: scope}
		return err
	})
	cancelHello()
	if c.helloDone != nil {
		c.helloDone()
	}
	if err != nil {
		c.srv.log.Warn("wsserver: handshake failed", "err", err)
		return
	}
	if c.stranger {
		c.pairOnce(ctx)
		return
	}
	c.srv.log.Info("wsserver: connected", "device", c.caller.Device, "label", h.Label)
	defer c.srv.log.Info("wsserver: disconnected", "device", c.caller.Device)
	// Pushes for this device go out here for as long as the socket is up.
	attached := c.srv.attach(c)
	defer c.srv.detach(c)
	if !attached {
		return
	}

	ctx, stopKeepalive := context.WithCancel(ctx)
	defer stopKeepalive()
	go c.ws.RunKeepalive(ctx, wsconn.Keepalive{
		Interval: pingInterval, Timeout: pingTimeout,
		Name: "wsserver", Log: c.srv.log, Attrs: []any{"device", c.caller.Device},
	})

	// Calls run concurrently, so a slow one never holds up
	// the next; ctx ends them when the socket goes.
	calls := rpc.NewInflight()
	for {
		msg, err := c.ws.ReadJSON(ctx)
		if err != nil {
			if !isNormalClose(err) {
				c.srv.log.Warn("wsserver: read failed", "err", err)
			}
			return
		}
		// An unknown method answers with an error, never by closing: a client
		// from a newer build calling something this one has not learned must
		// not lose its connection over it.
		if !calls.Go(func() { c.reply(ctx, c.srv.cfg.Router.Dispatch(ctx, c.caller, msg)) }) {
			c.reply(ctx, rpc.Busy(msg))
		}
	}
}

// pairOnce serves an unpaired device's one call, which must be an Unpaired
// method; the pairing code inside it is the real guard. The device then
// reconnects as paired, so a stranger never holds a live socket here.
func (c *conn) pairOnce(ctx context.Context) {
	c.srv.log.Info("wsserver: pairing over passthrough", "device", c.caller.Device)
	read, cancel := context.WithTimeout(ctx, pairTimeout)
	msg, err := c.ws.ReadJSON(read)
	cancel()
	if err != nil {
		return
	}
	if !c.srv.cfg.Router.Unpaired(msg.Method) {
		c.reply(ctx, jsonrpc.Fail(msg.ID, jsonrpc.CodeDenied, "this device is not paired with the host"))
		return
	}
	// Only the key that signed this hello may be paired on it.
	var joiner struct {
		DeviceID device.ID `json:"device_id"`
	}
	if json.Unmarshal(msg.Params, &joiner) != nil || joiner.DeviceID != c.caller.Device {
		c.reply(ctx, jsonrpc.Fail(msg.ID, jsonrpc.CodeDenied, "a device may only pair its own key"))
		return
	}
	c.reply(ctx, c.srv.cfg.Router.Dispatch(ctx, c.caller, msg))
	c.ws.Close()
}

func (c *conn) reply(ctx context.Context, reply *jsonrpc.Message) {
	if reply == nil {
		return
	}
	b, err := c.srv.cfg.Router.Encode(reply)
	if err == nil {
		err = c.ws.Write(ctx, websocket.MessageText, b)
	}
	if err != nil && ctx.Err() == nil {
		c.srv.log.Warn("wsserver: write failed", "err", err)
	}
}

// Send is the emit.Transport for this socket: a notification with no id. The
// context is fresh because a push is not tied to any request's lifetime;
// wsconn bounds the write itself.
func (c *conn) Send(_ device.ID, method string, payload []byte) error {
	return c.ws.SendJSON(context.Background(), &jsonrpc.Message{
		JSONRPC: jsonrpc.Version,
		Method:  method,
		Params:  payload,
	})
}

func isNormalClose(err error) bool {
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure ||
		status == websocket.StatusGoingAway ||
		errors.Is(err, context.Canceled)
}
