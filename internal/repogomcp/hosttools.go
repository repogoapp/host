package repogomcp

import (
	_ "embed"
	"encoding/json"
	"slices"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/rpc"
)

// The host's own methods marked rpc.Tool (chats first), offered to agents as
// they are: one tool per method, its params as the input, its result as the answer.

const hostToolsInstructions = " The `chats_*` tools list, read, rename, pin, resolve, stop and message " +
	"the chats on this environment, in every project, as the chat drawer does; `projects_list`, `git_*`, " +
	"`actions_*`, `schedules_list` and `tunnels_list` are the app's own reads and actions, which cards call too."

// hosttools.json is written from the router by `go test ./internal/rpc/conformance -update`.
//
//go:embed hosttools.json
var hostToolsFile []byte

var hostTools = func() []rpc.ToolDef {
	var defs []rpc.ToolDef
	if err := json.Unmarshal(hostToolsFile, &defs); err != nil {
		panic("repogomcp: hosttools.json: " + err.Error())
	}
	return defs
}()

// HostToolsFile is the embedded file, for the conformance test that writes it.
func HostToolsFile() []byte { return hostToolsFile }

// ownChatMethods would act on the calling chat itself: a turn messaging or
// stopping its own thread.
var ownChatMethods = []string{"chats.send", "chats.stop"}

func hostToolList() []tool {
	out := make([]tool, 0, len(hostTools))
	for _, d := range hostTools {
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
// The embedded list is the allowlist: nothing else on the router is reachable.
func callHost(router *rpc.Router, turn agent.RunningTurn, def rpc.ToolDef, args json.RawMessage) toolResult {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	if slices.Contains(ownChatMethods, def.Method) {
		var p struct {
			ChatID string `json:"chat_id"`
		}
		if json.Unmarshal(args, &p) == nil && p.ChatID == turn.ChatID {
			return failed("RepoGo " + def.Method + ": that is this chat; it can't message or stop itself")
		}
	}
	caller := rpc.Caller{Device: turn.Device, Scope: rpc.ScopeLocal}
	out, err := router.Call(turn.Ctx, caller, def.Method, args)
	if err != nil {
		return failed("RepoGo " + def.Method + ": " + err.Error())
	}
	return succeeded(out)
}
