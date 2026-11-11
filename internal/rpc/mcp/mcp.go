// Package mcp is the `mcp.*` methods: the MCP servers this host hands its
// agents, connected from a device and switched on per project, and their
// tools called for a device's cards.
package mcp

import (
	"context"
	"encoding/json"

	mcpcore "github.com/repogoapp/host/internal/mcp"
	"github.com/repogoapp/host/internal/outcome"
	"github.com/repogoapp/host/internal/rpc"
)

type Deps struct {
	MCP *mcpcore.Service
}

type ServerParams struct {
	ID string `json:"id"`
}

// ListParams' project is optional: with one, root is the project a server's
// switch lands on for it.
type ListParams struct {
	Project string `json:"project"`
}

type ListResult struct {
	Servers  []mcpcore.Server  `json:"servers" wire:"array"`
	Builtins []mcpcore.Builtin `json:"builtins" wire:"array"`
	Root     string            `json:"root,omitempty"`
}

type RenameParams struct {
	ServerParams
	Label string `json:"label"`
}

type SetEnabledParams struct {
	ServerParams
	Project string `json:"project"`
	Enabled bool   `json:"enabled"`
}

// CallParams names a tool of a server switched on for the project, by the
// key an agent knows the server by.
type CallParams struct {
	Project string          `json:"project"`
	Server  string          `json:"server"`
	Tool    string          `json:"tool"`
	Input   json.RawMessage `json:"input,omitempty"`
	// Read is a card's Data loading: only a read-only tool runs.
	Read bool `json:"read,omitempty"`
	// Confirmed is the user's yes; without it a destructive tool answers
	// CONFIRM instead of running.
	Confirmed bool `json:"confirmed,omitempty"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "mcp.list", d.list)
	rpc.Add(r, "mcp.rename", d.rename)
	rpc.Add(r, "mcp.set_enabled", d.setEnabled)
	rpc.Add(r, "mcp.remove", d.remove)
	// Plugins: ctx.mcp reaches the project's servers through it.
	rpc.Add(r, "mcp.call", d.call, rpc.Plugins)
}

func (d Deps) list(_ context.Context, _ rpc.Caller, a ListParams) (ListResult, error) {
	l := d.MCP.List(a.Project)
	return ListResult{Servers: l.Servers, Builtins: l.Builtins, Root: l.Root}, nil
}

func (d Deps) rename(_ context.Context, _ rpc.Caller, a RenameParams) (rpc.Ack, error) {
	return rpc.OK, d.MCP.Rename(a.ID, a.Label)
}

func (d Deps) setEnabled(_ context.Context, _ rpc.Caller, a SetEnabledParams) (rpc.Ack, error) {
	return rpc.OK, d.MCP.SetEnabled(a.ID, a.Project, a.Enabled)
}

func (d Deps) remove(_ context.Context, _ rpc.Caller, a ServerParams) (rpc.Ack, error) {
	return rpc.OK, d.MCP.Remove(a.ID)
}

func (d Deps) call(ctx context.Context, _ rpc.Caller, a CallParams) (outcome.Answer, error) {
	if a.Project == "" || a.Server == "" || a.Tool == "" {
		return outcome.Answer{}, rpc.ErrInvalidParams
	}
	return d.MCP.Call(ctx, a.Project, a.Server, a.Tool, a.Input, a.Read, a.Confirmed)
}
