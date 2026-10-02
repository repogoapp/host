package codex

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/chatwire"
	"github.com/repogo/host/internal/toollabel"
)

func labelOf(t *testing.T, name, input string) toollabel.Label {
	t.Helper()
	args, text := toollabel.Input(json.RawMessage(input))
	return label(name, args, text)
}

// cellInput is an `exec` call's input: the script as a JSON string.
func cellInput(t *testing.T, script string) string {
	t.Helper()
	b, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Codex's tools by the names its transcripts use. Function arguments arrive
// as a JSON string holding the object.
func TestToolLabel(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        toollabel.Label
	}{
		{"exec_command", `"{\"cmd\":\"git status --short\",\"workdir\":\"/repo\"}"`,
			toollabel.New("terminal", "git status --short", "git status --short", "Command attempted")},
		{"shell", `{"command":["bash","-lc","ls -la"]}`,
			toollabel.New("terminal", "ls -la", "ls -la", "Command attempted")},
		{"write_stdin", `"{\"session_id\":1,\"chars\":\"\"}"`,
			toollabel.New("terminal", "Checking a running command", "Checked a running command", "Checking a command failed")},
		{"write_stdin", `"{\"session_id\":1,\"chars\":\"y\\n\"}"`,
			toollabel.New("terminal", "Sending input to a command", "Sent input to a command", "Sending input failed")},
		{"apply_patch", `"*** Begin Patch\n*** Update File: /repo/App.swift\n@@\n-a\n+b\n*** End Patch"`,
			toollabel.New("square.and.pencil", "Editing App.swift", "Edited App.swift", "Edit attempted")},
		{"apply_patch", `"*** Begin Patch\n*** Add File: a.go\n*** Delete File: b.go\n*** End Patch"`,
			toollabel.New("square.and.pencil", "Editing 2 files", "Edited 2 files", "Edit attempted")},
		{"apply_patch", `{"input":"*** Begin Patch\n*** Update File: c.go\n*** End Patch"}`,
			toollabel.New("square.and.pencil", "Editing c.go", "Edited c.go", "Edit attempted")},
		{"apply_patch", `"*** Begin Patch\n*** End Patch"`,
			toollabel.New("square.and.pencil", "Editing files", "Edited files", "Edit attempted")},
		{"js", `"{\"title\":\"Check the page\"}"`,
			toollabel.New("chevron.left.forwardslash.chevron.right", "Running Check the page", "Ran Check the page", "Code failed")},
		{"wait", `"{\"cell_id\":\"c1\"}"`,
			toollabel.New("hourglass", "Waiting on running code", "Waited on running code", "Waiting failed")},
		{"sleep", `"{\"duration_ms\":5000}"`,
			toollabel.New("clock", "Waiting 5s", "Waited 5s", "Waiting failed")},
		{"sleep", `"{\"duration_ms\":200}"`,
			toollabel.New("clock", "Waiting", "Waited", "Waiting failed")},
		{"update_plan", `"{\"plan\":[]}"`,
			toollabel.New("checklist", "Updating the plan", "Updated the plan", "Updating the plan failed")},
		{"view_image", `"{\"path\":\"/tmp/shot.png\"}"`,
			toollabel.New("photo", "Viewing shot.png", "Viewed shot.png", "Viewing an image failed")},
		{"view_image", `"{}"`,
			toollabel.New("photo", "Viewing an image", "Viewed an image", "Viewing an image failed")},
		{"request_user_input", `"{}"`,
			toollabel.New("questionmark.bubble", "Asking you a question", "Asked you a question", "Question failed")},
		{"web_search", `"{\"query\":\"go 1.26 release\"}"`,
			toollabel.New("globe", "Searching go 1.26 release", "Searched go 1.26 release", "Web search attempted")},
		{"spawn_agent", `"{\"task_name\":\"/root/audit_relay\",\"agent_type\":\"worker\"}"`,
			toollabel.New("person.2", "Running Audit relay", "Ran Audit relay", "Subagent failed: Audit relay")},
		{"wait_agent", `"{}"`,
			toollabel.New("hourglass", "Waiting for subagents", "Waited for subagents", "Waiting for subagents failed")},
		{"send_message", `"{}"`,
			toollabel.New("arrow.turn.down.right", "Messaging a subagent", "Messaged a subagent", "Messaging a subagent failed")},
		{"resume_agent", `"{}"`,
			toollabel.New("arrow.clockwise", "Resuming a subagent", "Resumed a subagent", "Resuming a subagent failed")},
		{"interrupt_agent", `"{}"`,
			toollabel.New("stop.circle", "Interrupting a subagent", "Interrupted a subagent", "Interrupting a subagent failed")},
		{"create_goal", `"{\"objective\":\"Ship voice\"}"`,
			toollabel.New("flag", "Setting a goal", "Set a goal", "Setting a goal failed")},
		{"update_goal", `"{\"status\":\"complete\"}"`,
			toollabel.New("flag.checkered", "Marking the goal done", "Marked the goal done", "Updating the goal failed")},
		{"update_goal", `"{\"status\":\"blocked\"}"`,
			toollabel.New("flag", "Updating the goal", "Updated the goal", "Updating the goal failed")},
		{"clock__curr_time", `"{}"`,
			toollabel.New("clock", "Checking the time", "Checked the time", "Checking the time failed")},
		{"image_gen__imagegen", `"{\"prompt\":\"a fox\"}"`,
			toollabel.New("photo", "Generating an image", "Generated an image", "Image generation failed")},
		{"request_permissions", `"{}"`,
			toollabel.New("lock.shield", "Asking for permission", "Asked for permission", "Permission request failed")},
		{"wait_for_environment", `"{}"`,
			toollabel.New("hourglass", "Waiting for the environment", "Waited for the environment", "Waiting for the environment failed")},
		{"close_agent", `"{}"`,
			toollabel.New("xmark.circle", "Closing a subagent", "Closed a subagent", "Closing a subagent failed")},
		{"list_agents", `"{}"`,
			toollabel.New("list.bullet", "Listing subagents", "Listed subagents", "Listing subagents failed")},
		{"read", `{"path":"/repo/main.go"}`,
			toollabel.New("doc.text.magnifyingglass", "Reading main.go", "Read main.go", "Reading files attempted")},
	} {
		if got := labelOf(t, tc.name, tc.input); got != tc.want {
			t.Errorf("%s %s:\n got %+v\nwant %+v", tc.name, tc.input, got, tc.want)
		}
	}
}

func TestWebRun(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`{"search_query":[{"q":"iOS keychain sharing"},{"q":"app groups"}]}`, "Searched iOS keychain sharing"},
		{`{"image_query":[{"q":"red panda"}]}`, "Searched red panda"},
		{`{"open":[{"ref_id":"https://www.reddit.com/r/iOS"}]}`, "Read reddit.com"},
		{`{"open":[{"ref_id":"turn0search1"}]}`, "Read a page"},
		{`{"find":[{"ref_id":"turn0view0","pattern":"pricing"}]}`, "Found pricing in a page"},
		{`{"click":[{"ref_id":"turn1view0","id":79}]}`, "Opened a link"},
		{`{}`, "Used the web"},
	} {
		if got := labelOf(t, "web__run", tc.input).Labels.Completed; got != tc.want {
			t.Errorf("web__run %s = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// Code cells, as Codex writes them into `exec`: one call reads as that tool,
// several by the work they do, and a script this can't follow as code.
func TestCodeCellLabels(t *testing.T) {
	for _, tc := range []struct{ script, want string }{
		{`const r = await tools.exec_command({cmd:"git status --short","workdir":"/repo","yield_time_ms":10000}); text(r.output);`,
			"git status --short"},
		{`const r = await tools.web__run({search_query:[{q:"iOS keychain sharing"},{"q":"app groups"}],response_length:"long"}); text(r);`,
			"Searched iOS keychain sharing"},
		{`text(await tools.web__run({"open":[{"ref_id":"https://www.reddit.com/r/iOS"}]}));`,
			"Read reddit.com"},
		{`const r = await tools.web__run({click:[{ref_id:"turn1view0","id":79}]}); text(r);`,
			"Opened a link"},
		{"const patch = \"*** Begin Patch\\n*** Update File: /repo/App.swift\\n@@\\n-a\\n+b\\n*** End Patch\";\nconst r = await tools.apply_patch(patch); text(r);",
			"Edited App.swift"},
		// Checking a running command is not more work: the cell reads as its command.
		{"text(await tools.write_stdin({session_id:28931,chars:\"\",yield_time_ms:1000}));\ntext(await tools.exec_command({cmd:\"tail -8 /tmp/log\"}));",
			"tail -8 /tmp/log"},
		{"text(await tools.exec_command({cmd:\"pwd\"}));\ntext(await tools.exec_command({cmd:\"git status --short\"}));\ntext(await tools.exec_command({cmd:\"ls\"}));",
			"pwd and 2 more"},
		{"const patch = \"*** Begin Patch\\n*** Update File: /repo/App.swift\\n*** End Patch\";\nawait tools.apply_patch(patch);\ntext(await tools.exec_command({cmd:\"swift build\"}));",
			"Edited App.swift and ran a command"},
		{"await tools.apply_patch(\"*** Begin Patch\\n*** Update File: a.go\\n*** Add File: b.go\\n*** End Patch\");\nawait tools.exec_command({cmd:\"go test\"});\nawait tools.exec_command({cmd:\"go vet\"});",
			"Edited 2 files and ran 2 commands"},
		{`text(await tools.web__run({search_query:[{q:"swift"}]})); text(await tools.exec_command({cmd:"ls"}));`,
			"Searched the web and ran a command"},
		{`await tools.clock__curr_time({}); text(await tools.exec_command({cmd:"date"})); await tools.mcp__neon__run_sql({sql:"select 1"});`,
			"Ran a command and used 2 tools"},
		{`await tools.update_plan({plan:[]}); await tools.sleep({duration_ms:1000});`,
			"Used 2 tools"},
		{`const x = 1; text(x);`, "Ran code"},
		{`const r = await tools.exec_command(buildArgs()); text(r.output);`, "command"},
		{"await tools.mcp__node_repl__js({code: `const a = ${ {c: 1}.c };`, title: \"Check\"});",
			"Js · node.repl"},
	} {
		if got := labelOf(t, "exec", cellInput(t, tc.script)).Labels.Completed; got != tc.want {
			t.Errorf("exec %s\n got %q, want %q", tc.script, got, tc.want)
		}
	}
}

func TestCodeCellCalls(t *testing.T) {
	// Unquoted keys, escapes, comments, numeric separators and a trailing comma.
	calls := codeCellCalls("// @exec: {\"max_output_tokens\": 20000}\n" +
		"const r = await tools.exec_command({\n" +
		"  cmd: \"rg -n 'a\\\\|b' \\\"src dir\\\"\\nls\",\n" +
		"  workdir: '/repo', /* why */ yield_time_ms: 10_000,\n" +
		"});\ntext(r.output);")
	want := []cellCall{{Name: "exec_command", Input: map[string]any{
		"cmd": "rg -n 'a\\|b' \"src dir\"\nls", "workdir": "/repo", "yield_time_ms": float64(10000),
	}}}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("got %#v\nwant %#v", calls, want)
	}

	// A call's text inside a string is not a call.
	calls = codeCellCalls(`text(await tools.exec_command({cmd:"echo 'tools.web__run({})'"}));`)
	if len(calls) != 1 || calls[0].Name != "exec_command" ||
		calls[0].Input.(map[string]any)["cmd"] != "echo 'tools.web__run({})'" {
		t.Errorf("string contents read as a call: %#v", calls)
	}

	// A template keeps its interpolation as written.
	calls = codeCellCalls("await tools.mcp__node_repl__js({code: `const a = ${ {c: 1}.c };`, title: \"Check\"});")
	if len(calls) != 1 || calls[0].Input.(map[string]any)["code"] != "const a = ${ {c: 1}.c };" {
		t.Errorf("template: %#v", calls)
	}

	// An argument that isn't a literal is nil, and the call still counts.
	calls = codeCellCalls(`await tools.exec_command(buildArgs());`)
	if len(calls) != 1 || calls[0].Input != nil {
		t.Errorf("computed argument: %#v", calls)
	}

	// Unicode escapes, a surrogate pair's halves together, and null.
	calls = codeCellCalls(`tools.x({a:"\u00e9\u{1F600}\ud83d\ude00", b:null, c:[true,false,-1.5]})`)
	want = []cellCall{{Name: "x", Input: map[string]any{"a": "é😀😀", "b": nil, "c": []any{true, false, -1.5}}}}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("literals: got %#v\nwant %#v", calls, want)
	}

	// An unterminated script yields what it read without running past the end.
	if calls = codeCellCalls(`tools.exec_command({cmd:"ls`); len(calls) != 1 || calls[0].Input != nil {
		t.Errorf("unterminated: %#v", calls)
	}
}

func TestToolRow(t *testing.T) {
	p := &Provider{}
	row := func(name, input, output string) chatwire.Tool {
		return p.ToolRow(agent.ToolCall{Name: name, Input: json.RawMessage(input), Output: output})
	}

	// A code cell that calls one tool reads as that tool, labels and fields.
	r := row("exec", cellInput(t, `await tools.update_plan({plan:[{step:"Read",status:"completed"},{step:"Port",status:"in_progress"}]});`), "")
	if r.Name != "update_plan" || r.Labels.Completed != "Updated the plan" || len(r.Todos) != 2 || r.Todos[1].Content != "Port" {
		t.Errorf("plan cell: %+v", r)
	}
	r = row("exec", cellInput(t, `text(await tools.exec_command({cmd:"cat /repo/go.mod", workdir:"/repo"}));`), "")
	if r.Name != "exec_command" || r.Labels.Completed != "cat /repo/go.mod" {
		t.Errorf("command cell: %+v", r)
	}
	// A cell that calls several reads by its work and names every call, so
	// the chat's summary counts each one.
	r = row("exec", cellInput(t, "await tools.apply_patch(\"*** Begin Patch\\n*** Update File: a.go\\n*** End Patch\");\ntext(await tools.exec_command({cmd:\"go test\"}));"), "")
	if r.Name != "exec" || r.Labels.Completed != "Edited a.go and ran a command" ||
		!reflect.DeepEqual(r.Calls, []string{"apply_patch", "exec_command"}) {
		t.Errorf("two-call cell: %+v", r)
	}
	if r = row("exec", cellInput(t, `text(await tools.exec_command({cmd:"ls"}));`), ""); r.Calls != nil {
		t.Errorf("one-call cell names its calls: %+v", r)
	}

	r = row("request_user_input_async",
		`"{\"questions\":[{\"id\":\"q1\",\"question\":\"Which branch?\",\"options\":[\"main\",\"dev\"]}]}"`, "")
	if string(r.Questions) != `[{"id":"q1","options":["main","dev"],"question":"Which branch?"}]` {
		t.Errorf("questions: %s", r.Questions)
	}

	r = row("spawn_agent", `"{\"task_name\":\"/root/audit\",\"agent_type\":\"worker\"}"`, "")
	if r.SubagentType != "worker" || r.Labels.Completed != "Ran Audit" {
		t.Errorf("spawn_agent: %+v", r)
	}

	r = row("view_image", `"{\"path\":\"/tmp/shot.png\"}"`, "")
	if r.File != "shot.png" {
		t.Errorf("file: %+v", r)
	}

	if r = row("", "", "done"); r.Labels != nil || r.Name != "" {
		t.Errorf("result row: %+v", r)
	}
}

// A sheet shows a one-call code cell as that tool with the argument the cell
// wrote; anything else is the call as it was.
func TestResolve(t *testing.T) {
	p := &Provider{}
	got := p.Resolve(agent.ToolCall{CallID: "c", Name: "exec",
		Input: json.RawMessage(cellInput(t, `text(await tools.exec_command({cmd:"git status", workdir:"/repo"}));`))})
	if got.Name != "exec_command" || got.CallID != "c" {
		t.Errorf("name %q id %q", got.Name, got.CallID)
	}
	var args map[string]any
	if json.Unmarshal(got.Input, &args) != nil || args["cmd"] != "git status" || args["workdir"] != "/repo" {
		t.Errorf("input %s", got.Input)
	}

	two := agent.ToolCall{Name: "exec", Input: json.RawMessage(cellInput(t, "tools.a({});tools.b({});"))}
	if got := p.Resolve(two); got.Name != "exec" || string(got.Input) != string(two.Input) {
		t.Errorf("two-call cell changed: %+v", got)
	}
	plain := agent.ToolCall{Name: "exec_command", Input: json.RawMessage(`"{\"cmd\":\"ls\"}"`)}
	if got := p.Resolve(plain); !reflect.DeepEqual(got, plain) {
		t.Errorf("plain call changed: %+v", got)
	}
}
