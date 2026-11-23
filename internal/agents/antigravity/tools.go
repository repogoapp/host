package antigravity

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/toollabel"
)

// label names one of agy's tool calls the way RepoGo names Claude's and
// Codex's, from agy's tool name and its input. agy capitalises most argument
// names (Path, CommandLine), so they are read into the shared labels' words
// (path, command, url) first, which also gives a file row its file.
func label(tool string, input json.RawMessage) (toollabel.Label, map[string]any, bool) {
	raw, _ := toollabel.Input(input)
	args := map[string]any{}
	for k, v := range raw {
		args[k] = v
	}
	text := func(key string) string { return toollabel.String(raw, key) }
	if p := text("Path"); p != "" {
		args["path"] = p
	}
	if name, ok := mcpName(strings.TrimPrefix(tool, "Run ")); ok && !known[tool] {
		return toollabel.Shared(name, args), args, true
	}
	switch toolOf(tool, raw) {
	case "read_file":
		return toollabel.Read(args), args, true
	case "write_file", "edit_file":
		return toollabel.Edit(args), args, true
	case "run_command":
		// agy's own summary of the command, as Claude's Bash description is.
		args["command"], args["description"] = text("CommandLine"), text("toolSummary")
		return toollabel.Shell(args), args, true
	case "search_web":
		return toollabel.WebSearch(text("query")), args, true
	case "read_url_content":
		args["url"] = firstOf(text("Url"), text("url"))
		return toollabel.WebFetch(args), args, true
	case "ask_question":
		return toollabel.Question(args), args, true
	case "invoke_subagent":
		args["description"], args["prompt"] = text("Role"), text("Prompt")
		return toollabel.Subagent(args), args, true
	case "run_workflow":
		args["name"], args["script"] = text("Description"), text("Script")
		return toollabel.Workflow(args), args, true
	case "manage_task":
		return task(text("Action")), args, true
	case "schedule":
		return toollabel.New("clock", "Scheduling", "Scheduled", "Scheduling attempted"), args, true
	case "send_message":
		return toollabel.New("paperplane", "Messaging "+toollabel.Preview(text("Recipient"), 30, "an agent"),
			"Messaged "+toollabel.Preview(text("Recipient"), 30, "an agent"), "Message attempted"), args, true
	case "define_subagent", "manage_subagents", "swarm":
		return toollabel.New("person.3", "Managing agents", "Managed agents", "Managing agents attempted"), args, true
	}
	return toollabel.Shared(tool, args), args, true
}

// known is agy's own tools, as its ACP calls and its logs name them.
var known = map[string]bool{
	"read_file": true, "write_file": true, "edit_file": true, "run_command": true, "search_web": true,
	"read_url_content": true, "ask_question": true, "invoke_subagent": true, "run_workflow": true,
	"manage_task": true, "schedule": true, "send_message": true, "define_subagent": true,
	"manage_subagents": true, "swarm": true,
}

// toolOf is the agy tool a call ran. Chats RepoGo ran before calls were kept
// by their tool's name recorded agy's title instead ("File main.go", "ls -1"),
// so for a name agy doesn't use, the tool is told by its arguments.
func toolOf(name string, args map[string]any) string {
	if known[name] {
		return name
	}
	// agy titles a tool it has no words for "Run <tool>".
	if tool := strings.TrimPrefix(name, "Run "); known[tool] {
		return tool
	}
	has := func(key string) bool { _, ok := args[key]; return ok }
	switch {
	case has("CommandLine"):
		return "run_command"
	case has("Path") && has("Content"):
		return "write_file"
	case has("Path") && (has("Target") || has("Replacement")):
		return "edit_file"
	case has("Path"):
		return "read_file"
	case has("questions"):
		return "ask_question"
	case has("Prompt") && has("Role"):
		return "invoke_subagent"
	case has("Action") && has("TaskId"):
		return "manage_task"
	case has("query"):
		return "search_web"
	}
	return name
}

// task is a background task (a long command) agy checks on or stops.
func task(action string) toollabel.Label {
	switch strings.ToLower(action) {
	case "kill", "stop", "cancel", "terminate":
		return toollabel.New("stop.circle", "Stopping task", "Stopped task", "Stopping task attempted")
	case "input", "send_input", "write":
		return toollabel.New("keyboard", "Sending input", "Sent input", "Sending input attempted")
	}
	return toollabel.New("list.bullet.rectangle", "Checking task", "Checked task", "Checking task attempted")
}

// repogoServer is RepoGo's MCP server as agy names its tools: repogo, or
// repogo with the id a terminal's config gives it (repogo_6f15d105).
var repogoServer = regexp.MustCompile(`^` + agent.ToolsServer + `(_[0-9a-f]{8})?_(.+)$`)

// mcpName is an MCP tool's name as the shared labels read it,
// mcp__<server>__<tool>, from agy's mcp_<server>_<tool>. A server name may
// hold underscores too, so RepoGo's own is matched whole and any other is
// taken to end at the first underscore.
func mcpName(tool string) (string, bool) {
	rest, ok := strings.CutPrefix(tool, "mcp_")
	if !ok || rest == "" {
		return "", false
	}
	if m := repogoServer.FindStringSubmatch(rest); m != nil {
		return "mcp__" + agent.ToolsServer + "__" + m[2], true
	}
	server, name, ok := strings.Cut(rest, "_")
	if !ok || name == "" {
		return "mcp__" + agent.ToolsServer + "__" + rest, true
	}
	return "mcp__" + server + "__" + name, true
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
