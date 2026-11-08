package repogomcp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/outcome"
	"github.com/repogoapp/host/internal/plugins"
)

// Plugins are TypeScript in the environment's `~/.repogo/plugins/<name>/app.ts`,
// for every project, run by the plugins server (internal/plugins). These tools
// reach them from a turn.

const pluginsInstructions = " `plugins` lists the environment's plugins (`~/.repogo/plugins`), which every project " +
	"can call, and their methods; `plugin_call` runs one; `plugins_logs` reads what they wrote, and `plugins_urls` " +
	"the URLs of methods served over HTTP."

var pluginsTool = tool{
	Name: "plugins",
	Description: "List the environment's plugins (~/.repogo/plugins), which every project can use, " +
		"each with its `errors` in the last hour, the `page` that shows it, " +
		"and a line per method: `effect` (read, write or destructive), `stream` for a read that sends items, " +
		"`asks` when it has an approve question, and `input`, its fields " +
		"with optional ones ending in `?`; and `crons`, each line of ~/.repogo/crons.json with its error or next and last run. " +
		"With `plugin`, that plugin in full: its folder, each method's input and output JSON Schema, `approve` and `approveUpTo`, its storage, " +
		"the pages that show its methods (`target` to open with page: and the `params` to add) and the cron lines naming it. " +
		"Call this, and with the plugin's name for its schemas, before plugin_call; again after writing or changing a plugin or crons.json.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plugin": map[string]any{"type": "string", "description": "One plugin's name, to answer it in full"},
		},
	},
}

var pluginCallTool = tool{
	Name: "plugin_call",
	Description: "Run one method of a plugin this project can use and return its result. " +
		"`method` is `<plugin>.<method>` as `plugins` lists it, such as `plans.move`; " +
		"`input` must match that method's input schema. A method that asks first (`asks`) puts its question to the user " +
		"in the chat and waits, unless this chat's mode is less strict than its `approveUpTo`: it runs on their yes, " +
		"and answers that they declined on their no. In plan mode only reads run.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"method": map[string]any{"type": "string", "description": "`<plugin>.<method>`, as listed by `plugins`"},
			"input":  map[string]any{"type": "object", "description": "The method's input"},
		},
		"required": []string{"method"},
	},
}

// pluginsTools answers `plugins` and `plugin_call` for the turn's folder.
func pluginsTools(service *plugins.Service, turn agent.RunningTurn, name string, args json.RawMessage) toolResult {
	if name == pluginsTool.Name {
		var p struct {
			Plugin string `json:"plugin"`
		}
		_ = json.Unmarshal(args, &p)
		answer, err := service.List(turn.Ctx, turn.Cwd, p.Plugin)
		if err != nil || answer.Error != nil {
			return pluginResult(answer, err)
		}
		// As the host wrote it: compact, where succeeded would indent it threefold.
		return toolResult{Content: []content{{Type: "text", Text: string(answer.Result)}}}
	}
	var p struct {
		Method string          `json:"method"`
		Input  json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.Method == "" {
		return failed("RepoGo plugins: plugin_call needs a method, such as {\"method\":\"plans.list\",\"input\":{}}")
	}
	// Planning changes nothing: only reads run, asked or not.
	if turn.Config.Mode == "plan" {
		if m, ok, err := service.Method(turn.Ctx, turn.Cwd, p.Method); err == nil && ok && m.Effect != "read" {
			return failed("RepoGo: this chat is in plan mode, so plugin_call runs only reads, and " + p.Method + " is a " + m.Effect +
				". Put it in the plan; it can run once the user leaves plan mode.")
		}
	}
	// An approve is the user's to answer, never the agent's: its permissions may
	// let every tool call through. The question goes to the chat as an approval,
	// and the call waits on it, unless the chat's mode is past the method's approveUpTo.
	origin := plugins.Origin{Caller: plugins.CallerAgent, Chat: turn.ChatID}
	answer, err := service.Call(turn.Ctx, turn.Cwd, p.Method, p.Input, nil, false, origin)
	if err != nil || answer.Error == nil || answer.Error.Code != "CONFIRM" {
		return pluginResult(answer, err)
	}
	if m, ok, err := service.Method(turn.Ctx, turn.Cwd, p.Method); err == nil && ok && !asksIn(m.ApproveUpTo, turn.Config.PermissionMode) {
		return pluginResult(service.Call(turn.Ctx, turn.Cwd, p.Method, p.Input, nil, true, origin))
	}
	if turn.Ask == nil {
		return failed("CONFIRM: " + answer.Error.Message + " " + p.Method + " asks the user first, and this chat can't ask. " +
			"Draw a Button with action=\"plugin:" + p.Method + "\" and a Param per input field; the user's tap asks this question and runs it.")
	}
	switch approve(turn, p.Method, p.Input, answer.Error.Message) {
	case approveYes:
		return pluginResult(service.Call(turn.Ctx, turn.Cwd, p.Method, p.Input, nil, true, origin))
	case approveNo:
		return failed("The user declined " + p.Method + " (\"" + answer.Error.Message + "\"). It didn't run; don't call it again unless they ask.")
	default:
		return failed("Nobody answered " + p.Method + "'s question (\"" + answer.Error.Message + "\"), so it didn't run.")
	}
}

// modeRank orders the chat's permission modes from strictest.
var modeRank = map[agent.PermissionMode]int{
	agent.PermissionApprovalRequired: 0, agent.PermissionAutoAcceptEdits: 1, agent.PermissionFullAccess: 2,
}

// asksIn is whether a method that asks up to upTo asks in a chat's mode: in
// that mode and every stricter one. A mode the host can't tell asks, and an
// unset upTo is full access, every mode.
func asksIn(upTo string, mode agent.PermissionMode) bool {
	at, known := modeRank[mode]
	if !known {
		return true
	}
	limit, set := modeRank[agent.PermissionMode(upTo)]
	if !set {
		limit = modeRank[agent.PermissionFullAccess]
	}
	return at <= limit
}

type approveAnswer int

const (
	approveUnanswered approveAnswer = iota
	approveYes
	approveNo
)

const (
	approveYesID = "allow-once"
	approveNoID  = "reject"
)

// approve puts a method's question on the turn's chat, as an agent's tool
// approval is, and waits for the user's pick. Its own call id, so the prompt
// carries the plugin's question rather than the plugin_call row's title.
func approve(turn agent.RunningTurn, method string, input json.RawMessage, question string) approveAnswer {
	detail, _ := json.Marshal(map[string]json.RawMessage{"method": mustJSON(method), "input": orEmpty(input)})
	answer, err := turn.Ask(turn.Ctx, agent.Approval{
		CallID: "plugin-approve:" + randomID(), Title: question, Kind: "plugin", Input: detail,
		Options: []agent.ApprovalOption{
			{OptionID: approveYesID, Name: "Yes", Kind: "allow_once"},
			{OptionID: approveNoID, Name: "No", Kind: "reject_once"},
		},
	})
	if err != nil {
		return approveUnanswered
	}
	var picked string
	_ = json.Unmarshal(answer, &picked)
	if picked == approveYesID {
		return approveYes
	}
	return approveNo
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pluginResult passes a plugin's answer to the agent as is. A failed call keeps its
// code and field errors, so the agent can correct its input.
func pluginResult(answer outcome.Answer, err error) toolResult {
	if err != nil {
		if errors.Is(err, plugins.ErrNoPlugins) || errors.Is(err, plugins.ErrOff) {
			return failed("RepoGo " + err.Error())
		}
		return failed("RepoGo plugins: " + err.Error())
	}
	if answer.Error != nil {
		result := succeeded(answer.Error)
		result.IsError = true
		return result
	}
	return succeeded(answer.Result)
}
