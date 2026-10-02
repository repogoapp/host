// Package terminal is the `terminal.*` methods; the PTY lives in internal/terminal.
package terminal

import (
	"context"

	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/terminal"
)

type Deps struct {
	Terminals *terminal.Manager
}

type CreateParams struct {
	Cwd  string `json:"cwd"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

type CwdParams struct {
	Cwd string `json:"cwd"`
}

type ListResult struct {
	Sessions []terminal.Info `json:"sessions" wire:"array"`
}

type SessionParams struct {
	SessionID string `json:"session_id"`
}

type InputParams struct {
	SessionID string `json:"session_id"`
	Data      []byte `json:"data"`
}

type ResizeParams struct {
	SessionID string `json:"session_id"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

type AttachResult struct {
	terminal.Info
	BufferedOutput []byte `json:"buffered_output"`
}

func Register(r *rpc.Router, d Deps) {
	// create opens a shell in a project directory. Remote-reachable, unlike
	// turns.create: the cwd goes through containment rather than being named.
	rpc.Add(r, "terminal.create", d.create)
	// list is one project's tabs, or every session when no project is named.
	rpc.Add(r, "terminal.list", d.list)
	// subscribe keeps a tab strip live; renewing is calling it again.
	rpc.Add(r, "terminal.subscribe", d.subscribe)
	rpc.Add(r, "terminal.unsubscribe", d.unsubscribe)
	// attach subscribes and replays scrollback in the reply, so it cannot
	// interleave with the first live push.
	rpc.Add(r, "terminal.attach", d.attach)
	rpc.Add(r, "terminal.detach", d.detach)
	rpc.Add(r, "terminal.input", d.input)
	rpc.Add(r, "terminal.resize", d.resize)
	rpc.Add(r, "terminal.close", d.close)
}

func (d Deps) create(_ context.Context, c rpc.Caller, a CreateParams) (terminal.Info, error) {
	return d.Terminals.Create(c.Device, a.Cwd, a.Cols, a.Rows)
}

func (d Deps) list(_ context.Context, _ rpc.Caller, a CwdParams) (ListResult, error) {
	return ListResult{Sessions: d.Terminals.List(a.Cwd)}, nil
}

func (d Deps) subscribe(_ context.Context, c rpc.Caller, a CwdParams) (rpc.Ack, error) {
	return rpc.OK, d.Terminals.Subscribe(c.Device, a.Cwd)
}

func (d Deps) unsubscribe(_ context.Context, c rpc.Caller, a CwdParams) (rpc.Ack, error) {
	d.Terminals.Unsubscribe(c.Device, a.Cwd)
	return rpc.OK, nil
}

func (d Deps) attach(_ context.Context, c rpc.Caller, a SessionParams) (AttachResult, error) {
	buffered, info, err := d.Terminals.Attach(c.Device, a.SessionID)
	return AttachResult{Info: info, BufferedOutput: buffered}, err
}

func (d Deps) detach(_ context.Context, c rpc.Caller, a SessionParams) (rpc.Ack, error) {
	return rpc.OK, d.Terminals.Detach(c.Device, a.SessionID)
}

func (d Deps) input(_ context.Context, _ rpc.Caller, a InputParams) (rpc.Ack, error) {
	return rpc.OK, d.Terminals.Input(a.SessionID, a.Data)
}

func (d Deps) resize(_ context.Context, _ rpc.Caller, a ResizeParams) (rpc.Ack, error) {
	return rpc.OK, d.Terminals.Resize(a.SessionID, a.Cols, a.Rows)
}

func (d Deps) close(_ context.Context, _ rpc.Caller, a SessionParams) (rpc.Ack, error) {
	return rpc.OK, d.Terminals.Close(a.SessionID)
}
