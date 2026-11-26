package repogomcp

import (
	"encoding/json"
	"maps"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/remotehost"
	"github.com/repogoapp/host/internal/rpc"
)

// hostArg is the argument that sends a host tool to another environment.
const hostArg = "host"

const hostArgDescription = "Another environment to run this on: an id from hosts_list. Leave it out to run here. " +
	"Paths, chat ids and screen ids in the answer belong to that environment; pass the same host on follow-up calls."

const forwardInstructions = " Pass `host`, an id `hosts_list` answered, to run one of these tools on that environment instead of this one." +
	" Only the user switches an environment on, in the app; ask them to when the one they mean isn't listed."

// withHostArg is a tool's schema with the optional host argument, for a tool
// another host may run; the embedded schema is shared, so it's copied.
func withHostArg(d rpc.ToolDef) map[string]any {
	if d.NotRemote {
		return d.InputSchema
	}
	schema := maps.Clone(d.InputSchema)
	props, _ := schema["properties"].(map[string]any)
	props = maps.Clone(props)
	if props == nil {
		props = map[string]any{}
	}
	props[hostArg] = map[string]any{"type": "string", "description": hostArgDescription}
	schema["properties"] = props
	if schema["type"] == nil {
		schema["type"] = "object"
	}
	return schema
}

// splitHost takes the host argument out of a call's arguments: "" runs here.
func splitHost(args json.RawMessage) (string, json.RawMessage, error) {
	if len(args) == 0 || string(args) == "null" {
		return "", args, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return "", args, nil
	}
	raw, ok := fields[hostArg]
	if !ok {
		return "", args, nil
	}
	var host string
	if err := json.Unmarshal(raw, &host); err != nil {
		return "", nil, err
	}
	delete(fields, hostArg)
	rest, err := json.Marshal(fields)
	return host, rest, err
}

// forward runs a host tool on another environment for the turn: that host's
// hosts.call over SSH, with the calling chat filled from the turn, never the
// agent's arguments. Only an environment the user switched on takes one: the
// switch is theirs, in the app, and nothing here asks for it.
func (s *Server) forward(turn agent.RunningTurn, child bool, def rpc.ToolDef, host string, args json.RawMessage) toolResult {
	hosts := s.hosts()
	if hosts == nil {
		return failed("RepoGo " + def.Method + ": this environment can't reach others")
	}
	out, err := hosts.Call(turn.Ctx, host, remotehost.Request{Method: def.Method, Params: args, From: RemoteAgent{
		HostID: s.selfID(), ChatID: turn.ChatID,
		PermissionMode: string(turn.Config.PermissionMode), Mode: turn.Config.Mode, Child: child,
	}}, def.ReadOnly)
	// A development host's log: what was asked for, and how it went.
	if s.cfg.Log != nil {
		s.cfg.Log.Debug("hosts: forwarded", "method", def.Method, "host", host, "chat", turn.ChatID, "err", err)
	}
	if err != nil {
		return failed("RepoGo " + def.Method + " on " + host + ": " + err.Error())
	}
	// As a call here would answer: a screenshot as a picture, a screen read in lines.
	return hostResult(out)
}
