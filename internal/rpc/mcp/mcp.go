// Package mcp is the `mcp.*` methods: the MCP servers this host hands its
// agents, connected from a device and switched on per project.
package mcp

import (
	"context"

	mcpcore "github.com/repogo/host/internal/mcp"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	MCP *mcpcore.Service
}

type ServerParams struct {
	ID string `json:"id"`
}

// ListParams' project is optional: with one, the agents' own servers for it
// are listed too, and root is the project a server's switch lands on for it.
type ListParams struct {
	Project string `json:"project"`
}

type ListResult struct {
	Servers   []mcpcore.Server    `json:"servers" wire:"array"`
	Builtins  []mcpcore.Builtin   `json:"builtins" wire:"array"`
	Installed []mcpcore.Installed `json:"installed" wire:"array"`
	Root      string              `json:"root,omitempty"`
}

type ProbeParams struct {
	URL     string `json:"url"`
	Label   string `json:"label,omitempty"`
	Project string `json:"project"`
}

type ConnectParams struct {
	ProbeID string `json:"probe_id"`
	APIKey  string `json:"api_key,omitempty"`
}

type ServerResult struct {
	Server mcpcore.Server `json:"server"`
}

type OAuthStartParams struct {
	ProbeID  string `json:"probe_id"`
	ClientID string `json:"client_id,omitempty"`
	Loopback bool   `json:"loopback"`
}

type OAuthStartResult struct {
	AuthorizeURL string `json:"authorize_url"`
	State        string `json:"state"`
}

type OAuthFinishParams struct {
	ProbeID string `json:"probe_id"`
	Code    string `json:"code"`
	State   string `json:"state"`
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

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "mcp.list", d.list)
	rpc.Add(r, "mcp.probe", d.probe)
	rpc.Add(r, "mcp.connect", d.connect)
	rpc.Add(r, "mcp.oauth_start", d.oauthStart)
	// Detached: the code is spent once the token endpoint has it.
	rpc.Add(r, "mcp.oauth_finish", d.oauthFinish, rpc.Detached)
	rpc.Add(r, "mcp.rename", d.rename)
	rpc.Add(r, "mcp.set_enabled", d.setEnabled)
	rpc.Add(r, "mcp.remove", d.remove)
}

func (d Deps) list(_ context.Context, _ rpc.Caller, a ListParams) (ListResult, error) {
	l := d.MCP.List(a.Project)
	return ListResult{Servers: l.Servers, Builtins: l.Builtins, Installed: l.Installed, Root: l.Root}, nil
}

func (d Deps) probe(ctx context.Context, _ rpc.Caller, a ProbeParams) (mcpcore.Probe, error) {
	return d.MCP.Probe(ctx, a.URL, a.Label, a.Project)
}

func (d Deps) connect(_ context.Context, _ rpc.Caller, a ConnectParams) (ServerResult, error) {
	srv, err := d.MCP.Connect(a.ProbeID, a.APIKey)
	return ServerResult{Server: srv}, err
}

func (d Deps) oauthStart(ctx context.Context, _ rpc.Caller, a OAuthStartParams) (OAuthStartResult, error) {
	u, state, err := d.MCP.OAuthStart(ctx, a.ProbeID, a.ClientID, a.Loopback)
	return OAuthStartResult{AuthorizeURL: u, State: state}, err
}

func (d Deps) oauthFinish(ctx context.Context, _ rpc.Caller, a OAuthFinishParams) (ServerResult, error) {
	srv, err := d.MCP.OAuthFinish(ctx, a.ProbeID, a.Code, a.State)
	return ServerResult{Server: srv}, err
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
