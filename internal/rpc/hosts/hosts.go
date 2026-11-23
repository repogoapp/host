// Package hosts is the `hosts.*` methods: the other RepoGo hosts this one
// reaches over SSH, and the calls their agents make here; finding them lives
// in internal/remotehost, running a call in internal/repogomcp.
package hosts

import (
	"context"
	"encoding/json"

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

// CallParams is one tool call from an agent on another host: the host
// method the tool names, its params, and the chat calling.
type CallParams struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	From   From            `json:"from"`
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
	rpc.Add(r, "hosts.list", d.list, rpc.MCP, rpc.NotRemote, rpc.Tool("The other RepoGo environments this one "+
		"reaches over SSH, from ~/.ssh/config and the tailnet: each one's id and name, or why it can't be used "+
		"(offline, no SSH key, RepoGo not installed, not running or too old). Looks again when the last look is "+
		"over two minutes old; refresh looks again now."), rpc.Read)
	// Local only: the other host reaches it as `repogo call` over SSH, which
	// reads this machine's runtime token; SSH is what proved the user.
	rpc.Add(r, "hosts.call", d.call, rpc.Local)
}

func (d Deps) list(ctx context.Context, _ rpc.Caller, a ListParams) (remotehost.List, error) {
	return d.Hosts.List(ctx, a.Refresh), nil
}

func (d Deps) call(ctx context.Context, _ rpc.Caller, a CallParams) (CallResult, error) {
	out, err := d.Tools.RemoteCall(ctx, repogomcp.RemoteAgent(a.From), a.Method, a.Params)
	return CallResult{Result: out}, err
}
