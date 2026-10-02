// Package forward is the `forward.*` methods: a client's localhost connection
// carried to the same port on this host; the relaying lives in internal/forward.
package forward

import (
	"context"

	forwardcore "github.com/repogo/host/internal/forward"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	Fetcher *forwardcore.Fetcher
	Pipes   *forwardcore.Pipes
}

// PipeParams opens a pipe with the first bytes.
type PipeParams struct {
	// Named by the caller so it can register the pipe before the first byte.
	PipeID string `json:"pipe_id"`
	Port   int    `json:"port"`
	Data   []byte `json:"data"`
}

type SendParams struct {
	PipeID string `json:"pipe_id"`
	Data   []byte `json:"data"`
}

type CloseParams struct {
	PipeID string `json:"pipe_id"`
}

func Register(r *rpc.Router, d Deps) {
	// fetch proxies one buffered request. Remote-reachable; the guard is the
	// loopback-only dialer.
	rpc.Add(r, "forward.fetch", d.fetch)
	rpc.Add(r, "forward.pipe_open", d.pipeOpen)
	rpc.Add(r, "forward.pipe_send", d.pipeSend)
	rpc.Add(r, "forward.pipe_close", d.pipeClose)
}

func (d Deps) fetch(ctx context.Context, _ rpc.Caller, req forwardcore.Request) (forwardcore.Response, error) {
	return d.Fetcher.Fetch(ctx, req)
}

func (d Deps) pipeOpen(_ context.Context, c rpc.Caller, a PipeParams) (rpc.Ack, error) {
	return rpc.OK, d.Pipes.Open(c.Device, a.PipeID, a.Port, a.Data)
}

func (d Deps) pipeSend(_ context.Context, c rpc.Caller, a SendParams) (rpc.Ack, error) {
	return rpc.OK, d.Pipes.Send(c.Device, a.PipeID, a.Data)
}

func (d Deps) pipeClose(_ context.Context, c rpc.Caller, a CloseParams) (rpc.Ack, error) {
	d.Pipes.Close(c.Device, a.PipeID)
	return rpc.OK, nil
}
