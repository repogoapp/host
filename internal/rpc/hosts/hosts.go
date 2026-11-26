// Package hosts is the `hosts.*` methods: the other RepoGo hosts this one
// reaches over SSH, and the calls their agents make here; finding them lives
// in internal/remotehost, running a call in internal/repogomcp.
package hosts

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/repogoapp/host/internal/remotehost"
	"github.com/repogoapp/host/internal/repogomcp"
	"github.com/repogoapp/host/internal/rpc"
)

type Deps struct {
	Hosts *remotehost.Service
	Tools *repogomcp.Server
}

type ListParams struct {
	// Refresh looks again now, rather than reusing a look under two minutes old.
	Refresh bool `json:"refresh,omitempty"`
}

// ListingParams turns the list of the user's SSH connections on or off. On
// is their yes to reading ~/.ssh/config; off switches every host off too.
type ListingParams struct {
	On bool `json:"on"`
}

// AuthorizeParams switches one host on or off: ID is the id hosts.list
// answered for it. On, this host may dial it and agents may call it.
type AuthorizeParams struct {
	ID string `json:"id"`
	On bool   `json:"on"`
}

// CallParams is one tool call from an agent on another host: the host
// method the tool names, its params, and the chat calling.
type CallParams struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	From   From            `json:"from"`
	// To is the host the caller means to reach: this one's id, or nothing
	// runs. Empty from a caller that didn't say.
	To string `json:"to,omitempty"`
}

// From is the calling chat on the other host.
type From struct {
	HostID         string `json:"host_id"`
	ChatID         string `json:"chat_id"`
	PermissionMode string `json:"permission_mode,omitempty"`
	Mode           string `json:"mode,omitempty"`
	// Child is a chat another chat started there; it keeps a child's limits here.
	Child bool `json:"child,omitempty"`
}

// CallResult is the method's own result, as its tool answers it.
type CallResult struct {
	Result json.RawMessage `json:"result"`
}

func Register(r *rpc.Router, d Deps) {
	// Not remote: another host's agent listing this one's hosts would reach
	// a third, and calls stay one hop.
	rpc.Add(r, "hosts.list", d.list, rpc.MCP, rpc.NotRemote, rpc.Tool("The other environments the user switched "+
		"on for agents here, reached over SSH: each one's id, and whether it is ready or why it can't be used "+
		"(offline, SSH sign-in failed, RepoGo not installed, not running or too old). Only the user switches one "+
		"on, in the app under Other Environments; one that isn't here can't be called, so ask them to switch it "+
		"on. Looks again when the last look is over two minutes old; refresh looks again now."),
		rpc.Read, rpc.LiveOn("hosts.changed"))
	// Paired only: listing the user's SSH connections and switching one on
	// are their yes, a tap on a paired phone, never an agent's, a plugin's
	// or a shell's (as turns.respond).
	rpc.Add(r, "hosts.listing", d.listing, rpc.Paired)
	rpc.Add(r, "hosts.authorize", d.authorize, rpc.Paired)
	// Local only: the other host reaches it as `repogo call` over SSH, which
	// reads this machine's runtime token; SSH is what proved the user.
	rpc.Add(r, "hosts.call", d.call, rpc.Local)
}

// list is every connection for the user's own devices, and only the ones
// they switched on for an agent, a card or a plugin: the names in their SSH
// settings go no further than they chose.
func (d Deps) list(ctx context.Context, c rpc.Caller, a ListParams) (remotehost.Look, error) {
	look := d.Hosts.List(ctx, a.Refresh)
	if c.Scope != rpc.ScopeRemote || c.Agent != nil || c.Plugin != "" {
		look.Hosts = slices.DeleteFunc(look.Hosts, func(h remotehost.Host) bool { return !h.Authorized })
	}
	return look, nil
}

func (d Deps) listing(ctx context.Context, _ rpc.Caller, a ListingParams) (remotehost.Look, error) {
	return d.Hosts.SetListing(ctx, a.On)
}

func (d Deps) authorize(ctx context.Context, _ rpc.Caller, a AuthorizeParams) (remotehost.Host, error) {
	return d.Hosts.Authorize(ctx, a.ID, a.On)
}

func (d Deps) call(ctx context.Context, _ rpc.Caller, a CallParams) (CallResult, error) {
	out, err := d.Tools.RemoteCall(ctx, repogomcp.RemoteAgent(a.From), a.To, a.Method, a.Params)
	return CallResult{Result: out}, err
}
