package antigravity

import (
	"encoding/json"
	"testing"

	"github.com/repogoapp/host/internal/agent"
)

func TestAgysToolsReadLikeClaudesAndCodexs(t *testing.T) {
	cases := []struct {
		tool, input             string
		icon, active, completed string
	}{
		{"read_file", `{"Path":"/w/src/README.md","StartLine":1}`, "doc.text.magnifyingglass", "Reading README.md", "Read README.md"},
		{"write_file", `{"Path":"/w/hi.txt","Content":"hi"}`, "square.and.pencil", "Editing hi.txt", "Edited hi.txt"},
		{"edit_file", `{"Path":"/w/main.go","Target":"a","Replacement":"b"}`, "square.and.pencil", "Editing main.go", "Edited main.go"},
		{"run_command", `{"CommandLine":"ls -1","toolSummary":"List directory contents"}`, "terminal", "List directory contents", "List directory contents"},
		{"run_command", `{"CommandLine":"go test ./..."}`, "terminal", "go test ./...", "go test ./..."},
		{"search_web", `{"query":"t3code github"}`, "globe", "Searching t3code github", "Searched t3code github"},
		{"ask_question", `{"questions":[{"question":"Which colour?","options":["Red"]}]}`, "questionmark.bubble", "Which colour?", "Answered question"},
		{"invoke_subagent", `{"Role":"Code reviewer","Prompt":"Review the diff"}`, "person.2", "Running Code reviewer", "Ran Code reviewer"},
		{"manage_task", `{"Action":"status","TaskId":"t1"}`, "list.bullet.rectangle", "Checking task", "Checked task"},
		{"manage_task", `{"Action":"kill","TaskId":"t1"}`, "stop.circle", "Stopping task", "Stopped task"},
		{"mcp_repogo_6f15d105_chats_update", `{"chat_id":"c","title":"t"}`, "powerplug.fill", "Chats update · repogo", "Chats update · repogo"},
		{"mcp_repogo_plugin_call", `{"method":"m"}`, "powerplug.fill", "Plugin call · repogo", "Plugin call · repogo"},
		{"mcp_probe_ping", `{}`, "powerplug.fill", "Ping · probe", "Ping · probe"},
	}
	for _, c := range cases {
		l, _, ok := label(c.tool, json.RawMessage(c.input))
		if !ok || l.Icon != c.icon || l.Labels.Active != c.active || l.Labels.Completed != c.completed {
			t.Errorf("%s %s: %+v", c.tool, c.input, l)
		}
	}
}

// Calls RepoGo recorded under agy's titles, before they were kept by tool
// name, are told apart by their arguments.
func TestCallsRecordedByTitleAreToldByTheirArguments(t *testing.T) {
	cases := []struct{ title, input, completed string }{
		{"File .plans/LAYOUT.md", `{"Path":"/w/.plans/LAYOUT.md"}`, "Read LAYOUT.md"},
		{"File hi.txt", `{"Path":"/w/hi.txt","Content":"hi"}`, "Edited hi.txt"},
		{"File main.go", `{"Path":"/w/main.go","Target":"a","Replacement":"b"}`, "Edited main.go"},
		{"rg -n \"x\" apps", `{"CommandLine":"rg -n \"x\" apps","toolSummary":"Search the apps"}`, "Search the apps"},
		{"Run mcp_probe_ping", `{"toolSummary":"Ping probe server"}`, "Ping · probe"},
		{`"rank 2" elliptic curve on Web`, `{"query":"rank 2 elliptic curve"}`, "Searched rank 2 elliptic curve"},
		{"Run mcp_docs_search", `{"query":"x"}`, "Search · docs"},
		{"Run manage_task", `{"toolSummary":"Check on the build"}`, "Checked task"},
	}
	for _, c := range cases {
		l, _, _ := label(c.title, json.RawMessage(c.input))
		if l.Labels.Completed != c.completed {
			t.Errorf("%s: %q", c.title, l.Labels.Completed)
		}
	}
}

// A file row names its file, from agy's capitalised Path.
func TestAgysFileCallsNameTheirFile(t *testing.T) {
	p := New(agent.Dependencies{Root: t.TempDir()})
	defer p.Close()
	row := p.ToolRow(agent.ToolCall{CallID: "c", Name: "edit_file", Input: json.RawMessage(`{"Path":"/w/app/main.go"}`)})
	if row.Labels == nil || row.Labels.Completed != "Edited main.go" || row.File == "" {
		t.Fatalf("row %+v", row)
	}
}
