package stdiorpc

import (
	"context"
	"errors"
	"io"
)

// Connect speaks to a server that is already running, over conn: one JSON
// message per line each way. The client ends when conn does or ctx is
// cancelled; closing it closes conn and leaves the server running.
func Connect(ctx context.Context, conn io.ReadWriteCloser, version string, handlers Handlers) *Client {
	connCtx, cancel := context.WithCancel(ctx)
	c := newClient(connCtx, cancel, conn, handlers, version, false)
	go func() {
		stop := context.AfterFunc(connCtx, func() { _ = conn.Close() })
		if c.readMessages(conn) {
			c.fail(errors.New("stdiorpc: the server closed the connection"))
		}
		stop()
		_ = conn.Close()
		close(c.exited)
		c.mu.Lock()
		c.exit = Exit{Err: c.failure}
		for _, stop := range c.requests {
			stop()
		}
		c.mu.Unlock()
		cancel()
		close(c.notifications)
		close(c.done)
	}()
	return c
}
