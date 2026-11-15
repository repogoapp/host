package wsserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/wsconn"
)

// StreamFunc serves one media stream to a paired device until ctx ends:
// request is the device's first message after its hello, send writes one
// binary message (media), reply one text message (v as JSON), and input
// carries each later message the device sends, closing with the socket.
type StreamFunc func(ctx context.Context, c rpc.Caller, request []byte, send func([]byte) error,
	reply func(v any) error, input <-chan []byte) error

// streamInput is how many messages from the device wait for the stream; past
// it they are dropped, as a stuck stream's touches are stale anyway.
const streamInput = 256

// StreamFailed is the last text message on a /stream socket: why the stream
// ended, just before the socket closes, and a code when the client can act
// on it (a StreamFunc error with a StreamCode method).
type StreamFailed struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// serveStream reads the device's request, then sends the stream until the
// device closes the socket or the stream ends. It joins open, so revoking
// the device closes it, but never takes the device's route for calls and pushes.
func (c *conn) serveStream(ctx context.Context) {
	held := c.srv.holdStream(c)
	defer c.srv.releaseStream(c)
	if !held {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	read, cancelRead := context.WithTimeout(ctx, remoteHelloTimeout)
	_, request, err := c.ws.Read(read)
	cancelRead()
	if err != nil {
		return
	}
	// Reading sees the device's input, its close and its pongs.
	input := make(chan []byte, streamInput)
	go func() {
		defer cancel()
		defer close(input)
		for {
			typ, b, err := c.ws.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			select {
			case input <- b:
			default:
			}
		}
	}()
	go c.ws.RunKeepalive(ctx, wsconn.Keepalive{
		Interval: pingInterval, Timeout: pingTimeout,
		Name: "wsserver stream", Log: c.srv.log, Attrs: []any{"device", c.caller.Device},
	})

	c.srv.log.Info("wsserver: stream started", "device", c.caller.Device)
	err = c.srv.cfg.Stream(ctx, c.caller, request, func(b []byte) error {
		return c.ws.Write(ctx, websocket.MessageBinary, b)
	}, func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return c.ws.Write(ctx, websocket.MessageText, b)
	}, input)
	c.srv.log.Info("wsserver: stream ended", "device", c.caller.Device, "err", err)
	if err != nil && ctx.Err() == nil {
		failed := StreamFailed{Error: err.Error()}
		var coded interface{ StreamCode() string }
		if errors.As(err, &coded) {
			failed.Code = coded.StreamCode()
		}
		if b, merr := json.Marshal(failed); merr == nil {
			c.ws.Write(ctx, websocket.MessageText, b)
		}
	}
	c.ws.Close()
}

// holdStream counts a stream socket among the device's open ones, so a
// revocation closes it; false when the device was revoked during its hello.
func (s *Server) holdStream(c *conn) bool {
	s.mu.Lock()
	s.open[c] = struct{}{}
	s.mu.Unlock()
	_, err := s.cfg.Devices.Peer(c.caller.Device)
	return err == nil
}

func (s *Server) releaseStream(c *conn) {
	s.mu.Lock()
	delete(s.open, c)
	s.mu.Unlock()
}
