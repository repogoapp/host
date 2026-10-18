package repogomcp

import (
	"encoding/json"
	"errors"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/plugins"
)

// Plugins are TypeScript in `<project>/.repogo/plugins/<name>/app.ts`, or in
// `~/.repogo/plugins` for every project, run by the plugins server
// (internal/plugins). These tools reach them from a turn.

const pluginsInstructions = " `plugins` lists this project's plugins (`.repogo/plugins`, and the environment's own " +
	"in `~/.repogo/plugins`) and their methods; `plugin_call` runs one."

var pluginsTool = tool{
	Name: "plugins",
	Description: "List the plugins this project can use: its own in .repogo/plugins and the environment's in " +
		"~/.repogo/plugins (each plugin's `dir` says which), with each plugin's methods, " +
		"whether a method is a query (reads) or a mutation (writes), and the JSON Schema of its input; " +
		"and `pages`, where a project or plugin page already shows a query: its `target` to open with page:, " +
		"and the `params` to add to it. Call this before plugin_call, and again after writing or changing a plugin.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
}

var pluginCallTool = tool{
	Name: "plugin_call",
	Description: "Run one method of a plugin this project can use and return its result. " +
		"`method` is `<plugin>.<method>` as `plugins` lists it, such as `plans.move`; " +
		"`input` must match that method's input schema. A method with a `confirm` answers CONFIRM with its question " +
		"instead of running: put the question to the user, and call again with `confirmed: true` only once they agree.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"method":    map[string]any{"type": "string", "description": "`<plugin>.<method>`, as listed by `plugins`"},
			"input":     map[string]any{"type": "object", "description": "The method's input"},
			"confirmed": map[string]any{"type": "boolean", "description": "The user agreed to the method's confirm question"},
		},
		"required": []string{"method"},
	},
}

// pluginsTools answers `plugins` and `plugin_call` for the turn's folder.
func pluginsTools(service *plugins.Service, turn agent.RunningTurn, name string, args json.RawMessage) toolResult {
	if name == pluginsTool.Name {
		answer, err := service.List(turn.Ctx, turn.Cwd)
		return pluginResult(answer, err)
	}
	var p struct {
		Method    string          `json:"method"`
		Input     json.RawMessage `json:"input"`
		Confirmed bool            `json:"confirmed"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.Method == "" {
		return failed("RepoGo plugins: plugin_call needs a method, such as {\"method\":\"plans.list\",\"input\":{}}")
	}
	// A confirm guards what can't be undone from agents as from taps: an agent's
	// permissions may let every tool call through, so the user's yes is its own step.
	answer, err := service.Call(turn.Ctx, turn.Cwd, p.Method, p.Input, p.Confirmed)
	return pluginResult(answer, err)
}

// pluginResult passes a plugin's answer to the agent as is. A failed call keeps its
// code and field errors, so the agent can correct its input.
func pluginResult(answer plugins.Answer, err error) toolResult {
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
