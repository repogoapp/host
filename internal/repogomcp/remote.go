package repogomcp

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/rpc"
)

// RemoteAgent is the chat on another host whose agent calls a tool here,
// through that host running `repogo call hosts.call` over SSH. SSH proved the
// user; who the chat is, the other host says.
type RemoteAgent struct {
	HostID         string `json:"host_id"`
	ChatID         string `json:"chat_id"`
	PermissionMode string `json:"permission_mode,omitempty"`
	Mode           string `json:"mode,omitempty"`
	// Child is a chat another chat started there: it keeps a child's limits here.
	Child bool `json:"child,omitempty"`
}

// RemoteCall runs one host tool for an agent on another host, through the
// same allowlist and limits as this host's own agents.
func (s *Server) RemoteCall(ctx context.Context, from RemoteAgent, to, method string, params json.RawMessage) (json.RawMessage, error) {
	router := s.router()
	// Switched off here is off for another host's agents too.
	if router == nil || (s.cfg.On != nil && !s.cfg.On("")) {
		return nil, errkind.New(errkind.Unavailable, "RepoGo's tools are switched off on this environment")
	}
	// The caller checked which host answers here; one that changed since
	// isn't the one the user switched on for it.
	if to != "" && to != s.selfID() {
		return nil, errkind.New(errkind.Denied, "this isn't the environment that call was sent to")
	}
	if from.HostID == "" || from.ChatID == "" {
		return nil, errkind.New(errkind.Invalid, "from needs the calling host_id and chat_id")
	}
	i := slices.IndexFunc(hostTools, func(d rpc.ToolDef) bool { return d.Method == method })
	if i < 0 {
		return nil, errkind.New(errkind.Invalid, method+" isn't a tool agents may call")
	}
	caller := rpc.Caller{Scope: rpc.ScopeLocal, Agent: &rpc.AgentCaller{
		ChatID: from.ChatID, PermissionMode: from.PermissionMode, Mode: from.Mode,
		HostID: from.HostID, Child: from.Child,
	}}
	return runHostTool(ctx, router, caller, hostTools[i], params)
}
