package wsserver

import (
	"context"
	"errors"
	"time"

	"github.com/coder/websocket"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/release"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/wsconn"
)

const (
	pingInterval = 20 * time.Second
	pingTimeout  = 10 * time.Second
)

// conn is one client connection: handshake, then a dispatch loop.
type conn struct {
	srv *Server
	ws  *wsconn.Conn

	caller rpc.Caller
}

func newConn(srv *Server, ws *websocket.Conn) *conn {
	return &conn{srv: srv, ws: wsconn.Wrap(ws)}
}

func (c *conn) run(ctx context.Context) {
	defer c.ws.CloseNow()

	// Both auth paths share one challenge: a same-machine client answers with
	// the loopback token, a paired device signs the nonce.
	h, err := c.ws.Accept(ctx, c.srv.cfg.ServerID, release.Version, func(h handshake.Hello, signed []byte) error {
		scope, err := c.srv.authenticate(&h, signed)
		c.caller = rpc.Caller{Device: device.ID(h.DeviceID), Scope: scope}
		return err
	})
	if err != nil {
		c.srv.log.Warn("wsserver: handshake failed", "err", err)
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

	// Calls run concurrently, as over the relay, so a slow one never holds up
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
