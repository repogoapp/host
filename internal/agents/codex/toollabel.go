package codex

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/chatwire"
	"github.com/repogo/host/internal/toollabel"
)

// ToolRow is a Codex tool call on the chat lane (chatwire.Labeler). A code
// cell that calls one tool reads as that tool, so the chat still finds its
// todos, questions, browser calls and subagents.
func (p *Provider) ToolRow(call agent.ToolCall) chatwire.Tool {
	if call.Name == "" {
		return chatwire.Tool{}
	}
	name := call.Name
	args, text := toollabel.Input(call.Input)
	var calls []cellCall
	if name == "exec" {
		calls = codeCellCalls(text)
		if len(calls) == 1 {
			name = calls[0].Name
			args, _ = calls[0].Input.(map[string]any)
			text, _ = calls[0].Input.(string)
		}
	}
	row := chatwire.Labeled(name, label(name, args, text), args)
	if len(calls) > 1 {
		for _, c := range calls {
			row.Calls = append(row.Calls, c.Name)
		}
	}
	switch name {
	case "update_plan":
		row.Todos = chatwire.Todos(call.Output, args)
	case "request_user_input_async":
		if questions, ok := args["questions"]; ok {
			row.Questions, _ = json.Marshal(questions)
		}
	case "spawn_agent":
		row.SubagentType = strings.TrimSpace(toollabel.String(args, "agent_type"))
	}
	return row
}

// Resolve is the call as a sheet shows it: a code cell that calls one tool
// becomes that tool's call, with the argument the cell wrote.
func (p *Provider) Resolve(call agent.ToolCall) agent.ToolCall {
	if call.Name != "exec" {
		return call
	}
	_, script := toollabel.Input(call.Input)
	calls := codeCellCalls(script)
	if len(calls) != 1 {
		return call
	}
	call.Name = calls[0].Name
	call.Input = nil
	if calls[0].Input != nil {
		call.Input, _ = json.Marshal(calls[0].Input)
	}
	return call
}

// label names a call from its read input, so ToolRow can label a code cell
// that calls one tool as that tool.
func label(name string, args map[string]any, text string) toollabel.Label {
	switch name {
	case "shell", "shell_command", "local_shell", "exec_command":
		return toollabel.Shell(args)
	case "write_stdin":
		if toollabel.String(args, "chars") == "" {
			return toollabel.New("terminal", "Checking a running command", "Checked a running command", "Checking a command failed")
		}
		return toollabel.New("terminal", "Sending input to a command", "Sent input to a command", "Sending input failed")
	case "exec":
		return codeCell(text)
	case "web__run":
		return webRun(args)
	case "js":
		title := toollabel.Preview(toollabel.String(args, "title"), 40, "code")
		return toollabel.New("chevron.left.forwardslash.chevron.right", "Running "+title, "Ran "+title, "Code failed")
	case "wait":
		// A wait on a running code cell (`cell_id`); agents use wait_agent.
		return toollabel.New("hourglass", "Waiting on running code", "Waited on running code", "Waiting failed")
	case "sleep":
		span := ""
		if seconds := toollabel.Int(args, "duration_ms") / 1000; seconds > 0 {
			span = fmt.Sprintf(" %ds", seconds)
		}
		return toollabel.New("clock", "Waiting"+span, "Waited"+span, "Waiting failed")
	case "apply_patch":
		if text == "" {
			text = toollabel.String(args, "input")
		}
		return patch(text)
	case "update_plan":
		return toollabel.New("checklist", "Updating the plan", "Updated the plan", "Updating the plan failed")
	case "view_image":
		file := toollabel.FileName(toollabel.String(args, "path"), "an image")
		return toollabel.New("photo", "Viewing "+file, "Viewed "+file, "Viewing an image failed")
	case "request_user_input", "request_user_input_async":
		return toollabel.New("questionmark.bubble", "Asking you a question", "Asked you a question", "Question failed")
	case "spawn_agent":
		return toollabel.Subagent(args)
	case "wait_agent":
		return toollabel.New("hourglass", "Waiting for subagents", "Waited for subagents", "Waiting for subagents failed")
	case "send_input", "send_message", "followup_task":
		return toollabel.New("arrow.turn.down.right", "Messaging a subagent", "Messaged a subagent", "Messaging a subagent failed")
	case "resume_agent":
		return toollabel.New("arrow.clockwise", "Resuming a subagent", "Resumed a subagent", "Resuming a subagent failed")
	case "interrupt_agent":
		return toollabel.New("stop.circle", "Interrupting a subagent", "Interrupted a subagent", "Interrupting a subagent failed")
	case "close_agent":
		return toollabel.New("xmark.circle", "Closing a subagent", "Closed a subagent", "Closing a subagent failed")
	case "list_agents":
		return toollabel.New("list.bullet", "Listing subagents", "Listed subagents", "Listing subagents failed")
	case "create_goal":
		return toollabel.New("flag", "Setting a goal", "Set a goal", "Setting a goal failed")
	case "update_goal":
		if toollabel.String(args, "status") == "complete" {
			return toollabel.New("flag.checkered", "Marking the goal done", "Marked the goal done", "Updating the goal failed")
		}
		return toollabel.New("flag", "Updating the goal", "Updated the goal", "Updating the goal failed")
	case "get_goal":
		return toollabel.New("flag", "Checking the goal", "Checked the goal", "Checking the goal failed")
	case "curr_time", "clock__curr_time":
		return toollabel.New("clock", "Checking the time", "Checked the time", "Checking the time failed")
	case "imagegen", "image_gen__imagegen":
		return toollabel.New("photo", "Generating an image", "Generated an image", "Image generation failed")
	case "tool_search":
		return toollabel.ToolSearch(args, func(name string) toollabel.Label { return label(name, nil, "") })
	case "request_permissions":
		return toollabel.New("lock.shield", "Asking for permission", "Asked for permission", "Permission request failed")
	case "list_mcp_resources", "list_mcp_resource_templates":
		return toollabel.New("powerplug.fill", "Listing MCP resources", "Listed MCP resources", "Listing MCP resources failed")
	case "read_mcp_resource":
		return toollabel.New("powerplug.fill", "Reading an MCP resource", "Read an MCP resource", "Reading an MCP resource failed")
	case "send_message_to_user_async":
		return toollabel.New("bubble.left", "Messaging you", "Messaged you", "Messaging you failed")
	case "wait_for_environment":
		return toollabel.New("hourglass", "Waiting for the environment", "Waited for the environment", "Waiting for the environment failed")
	case "list_available_plugins_to_install":
		return toollabel.New("puzzlepiece.extension", "Listing plugins", "Listed plugins", "Listing plugins failed")
	case "request_plugin_install":
		return toollabel.New("puzzlepiece.extension", "Asking to install a plugin", "Asked to install a plugin", "Plugin install request failed")
	}
	return toollabel.Shared(name, args)
}

// codeCell labels an `exec` script: as the one tool it calls, else by the
// work its calls do; a script that can't be followed reads "Ran code".
func codeCell(script string) toollabel.Label {
	calls := codeCellCalls(script)
	if len(calls) == 0 {
		return toollabel.New("chevron.left.forwardslash.chevron.right", "Running code", "Ran code", "Code failed")
	}
	work := cellWork(calls)
	if len(work) == 1 {
		return callLabel(work[0])
	}
	return cellSummary(work)
}

func callLabel(call cellCall) toollabel.Label {
	args, _ := call.Input.(map[string]any)
	text, _ := call.Input.(string)
	return label(call.Name, args, text)
}

// cellWork drops the calls that only wait on or read back earlier work
// (checking a running command, waiting, the plan), unless that is all there is.
func cellWork(calls []cellCall) []cellCall {
	var work []cellCall
	for _, call := range calls {
		args, _ := call.Input.(map[string]any)
		switch call.Name {
		case "wait", "sleep", "update_plan":
			continue
		case "write_stdin":
			if toollabel.String(args, "chars") == "" {
				continue
			}
		}
		work = append(work, call)
	}
	if len(work) == 0 {
		return calls
	}
	return work
}

// cellSummary names a cell's several calls: one command and the count of the
// rest when all are commands, else each kind of work in one sentence, edits
// first ("Edited App.swift and ran 2 commands").
func cellSummary(calls []cellCall) toollabel.Label {
	var commands, webs, others int
	var files []string
	var first cellCall
	for _, call := range calls {
		switch call.Name {
		case "shell", "shell_command", "local_shell", "exec_command", "write_stdin":
			if commands == 0 {
				first = call
			}
			commands++
		case "apply_patch":
			text, _ := call.Input.(string)
			if args, ok := call.Input.(map[string]any); ok {
				text = toollabel.String(args, "input")
			}
			touched := patchFiles(text)
			if len(touched) == 0 {
				touched = []string{""}
			}
			files = append(files, touched...)
		case "web__run":
			webs++
		default:
			others++
		}
	}
	if commands == len(calls) {
		line := callLabel(first).Labels.Completed + fmt.Sprintf(" and %d more", commands-1)
		return toollabel.New("terminal", line, line, "Command attempted")
	}

	type phrase struct{ active, done string }
	var phrases []phrase
	icon := ""
	if len(files) > 0 {
		what := fmt.Sprintf("%d files", len(files))
		if len(files) == 1 {
			what = toollabel.FileName(files[0], "a file")
		}
		phrases = append(phrases, phrase{"editing " + what, "edited " + what})
		icon = "square.and.pencil"
	}
	if webs > 0 {
		phrases = append(phrases, phrase{"searching the web", "searched the web"})
		if icon == "" {
			icon = "globe"
		}
	}
	if commands > 0 {
		what := "a command"
		if commands > 1 {
			what = fmt.Sprintf("%d commands", commands)
		}
		phrases = append(phrases, phrase{"running " + what, "ran " + what})
		if icon == "" {
			icon = "terminal"
		}
	}
	if others > 0 {
		what := "a tool"
		if others > 1 {
			what = fmt.Sprintf("%d tools", others)
		}
		phrases = append(phrases, phrase{"using " + what, "used " + what})
		if icon == "" {
			icon = "wrench.and.screwdriver"
		}
	}
	active, done := make([]string, len(phrases)), make([]string, len(phrases))
	for i, p := range phrases {
		active[i], done[i] = p.active, p.done
	}
	return toollabel.New(icon, sentence(active), sentence(done), "Code failed")
}

// sentence joins phrases as "a, b and c", capitalized.
func sentence(phrases []string) string {
	s := phrases[len(phrases)-1]
	if len(phrases) > 1 {
		s = strings.Join(phrases[:len(phrases)-1], ", ") + " and " + s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// webRun is Codex's web tool: `search_query: [{q}]` searches, `open:
// [{ref_id}]` opens a URL or an earlier result, `find` looks in a page.
func webRun(args map[string]any) toollabel.Label {
	first := func(key, field string) string {
		if items := toollabel.Dicts(args, key); len(items) > 0 {
			return toollabel.String(items[0], field)
		}
		return ""
	}
	if query := first("search_query", "q"); query != "" {
		return toollabel.WebSearch(query)
	}
	if query := first("image_query", "q"); query != "" {
		return toollabel.WebSearch(query)
	}
	if ref := first("open", "ref_id"); ref != "" {
		site := toollabel.Host(ref)
		if site == "" {
			site = "a page"
		}
		return toollabel.New("globe", "Reading "+site, "Read "+site, "Opening a page failed")
	}
	if pattern := first("find", "pattern"); pattern != "" {
		p := toollabel.Preview(pattern, 40, "text")
		return toollabel.New("globe", "Finding "+p+" in a page", "Found "+p+" in a page", "Finding in a page failed")
	}
	if len(toollabel.Dicts(args, "click")) > 0 {
		return toollabel.New("globe", "Opening a link", "Opened a link", "Opening a link failed")
	}
	return toollabel.New("globe", "Using the web", "Used the web", "Web lookup failed")
}

// patch labels an apply_patch by the file it touches, or how many.
func patch(text string) toollabel.Label {
	files := patchFiles(text)
	what := "files"
	switch {
	case len(files) == 1:
		what = toollabel.FileName(files[0], "file")
	case len(files) > 1:
		what = fmt.Sprintf("%d files", len(files))
	}
	return toollabel.New("square.and.pencil", "Editing "+what, "Edited "+what, "Edit attempted")
}

// patchFiles is the files an apply_patch touches, in order.
func patchFiles(text string) []string {
	var files []string
	for _, line := range strings.Split(text, "\n") {
		for _, prefix := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: "} {
			if file, ok := strings.CutPrefix(line, prefix); ok {
				files = append(files, strings.TrimSpace(file))
			}
		}
	}
	return files
}
