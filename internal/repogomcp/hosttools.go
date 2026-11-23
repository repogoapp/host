package repogomcp

import (
	"context"
	_ "embed"
	"encoding/json"
	"slices"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/rpc"
)

// The host's own methods marked rpc.Tool (chats first), offered to agents as
// they are: one tool per method, its params as the input, its result as the answer.

const hostToolsInstructions = " The `chats_*` tools list, read, rename, pin, resolve, stop and message " +
	"the chats on this environment, in every project, as the chat drawer does, and start another agent's chat " +
	"(`agents_list`, `chats_start`) and wait for its answer (`chats_wait`); `projects_list`, " +
	"`schedules_list` and `tunnels_list` are the app's own reads; " +
	"`reminders_*` notify the user's phones at a set time, set only when the user asks; " +
	"the `screens_*` tools drive this environment's iOS simulators and Android emulators: read the screen " +
	"(`screens_elements`), tap, swipe, type, press keys and buttons, rotate, fold, and save screenshots."

// hosttools.json is written from the router by `go test ./internal/rpc/conformance -update`.
//
//go:embed hosttools.json
var hostToolsFile []byte

// hostTools is the methods marked rpc.MCP; a Tool without it (actions, git,
// limits) is for cards and plugins only.
var hostTools = func() []rpc.ToolDef {
	var defs []rpc.ToolDef
	if err := json.Unmarshal(hostToolsFile, &defs); err != nil {
		panic("repogomcp: hosttools.json: " + err.Error())
	}
	return slices.DeleteFunc(defs, func(d rpc.ToolDef) bool { return !d.MCP })
}()

// HostToolsFile is the embedded file, for the conformance test that writes it.
func HostToolsFile() []byte { return hostToolsFile }

// hostToolList is what a session is offered: a child isn't offered what the
// router would refuse it (rpc.NotFromChild).
func hostToolList(child bool) []tool {
	out := make([]tool, 0, len(hostTools))
	for _, d := range hostTools {
		if child && d.NotFromChild {
			continue
		}
		t := tool{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema}
		if d.ReadOnly {
			t.Annotations = &annotations{ReadOnlyHint: true}
		}
		out = append(out, t)
	}
	return out
}

func hostTool(name string) (rpc.ToolDef, bool) {
	i := slices.IndexFunc(hostTools, func(d rpc.ToolDef) bool { return d.Name == name })
	if i < 0 {
		return rpc.ToolDef{}, false
	}
	return hostTools[i], true
}

// callHost runs one host method for the turn, as the phone that sent it would.
func callHost(router *rpc.Router, turn agent.RunningTurn, child bool, def rpc.ToolDef, args json.RawMessage) toolResult {
	caller := rpc.Caller{Device: turn.Device, Scope: rpc.ScopeLocal, Agent: &rpc.AgentCaller{
		ChatID: turn.ChatID, PermissionMode: string(turn.Config.PermissionMode), Mode: turn.Config.Mode, Child: child,
	}}
	out, err := runHostTool(turn.Ctx, router, caller, def, args)
	if err != nil {
		return failed("RepoGo " + def.Method + ": " + err.Error())
	}
	return succeeded(out)
}

// runHostTool is every agent's way onto the router, this host's and another's
// (RemoteCall). The router decides what the caller may call (rpc.Spec.allows).
func runHostTool(ctx context.Context, router *rpc.Router, caller rpc.Caller, def rpc.ToolDef, args json.RawMessage) (json.RawMessage, error) {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	return router.Call(ctx, caller, def.Method, args)
}
