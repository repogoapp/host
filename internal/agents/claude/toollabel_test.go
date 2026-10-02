package claude

import (
	"encoding/json"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/toollabel"
)

func toolLabel(name, input string) toollabel.Label {
	args, _ := toollabel.Input(json.RawMessage(input))
	return label(name, args)
}

// Claude's tools by the names its transcripts use; anything else falls to the
// shared vocabulary.
func TestToolLabel(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        toollabel.Label
	}{
		{"Read", `{"file_path":"/repo/apps/host/go.mod"}`,
			toollabel.New("doc.text.magnifyingglass", "Reading go.mod", "Read go.mod", "Reading files attempted")},
		{"Edit", `{"file_path":"/repo/Chat.swift"}`,
			toollabel.New("square.and.pencil", "Editing Chat.swift", "Edited Chat.swift", "Editing file attempted")},
		{"MultiEdit", `{"file_path":"/repo/a.go"}`,
			toollabel.New("square.and.pencil", "Editing a.go", "Edited a.go", "Editing file attempted")},
		{"Write", `{"file_path":"/repo/new.md"}`,
			toollabel.New("square.and.pencil", "Editing new.md", "Edited new.md", "Editing file attempted")},
		{"Bash", `{"command":"go test ./...","description":"Run the host tests"}`,
			toollabel.New("terminal", "Run the host tests", "Run the host tests", "Command attempted")},
		{"Bash", `{"command":"git status"}`,
			toollabel.New("terminal", "git status", "git status", "Command attempted")},
		{"Grep", `{"pattern":"TODO"}`,
			toollabel.New("magnifyingglass", "Searching TODO", "Searched TODO", "Grep search attempted")},
		{"Glob", `{"pattern":"*.swift"}`,
			toollabel.New("doc.text.magnifyingglass", "Searching *.swift", "Searched *.swift", "Glob search attempted")},
		{"LS", `{"path":"/a/b/c"}`,
			toollabel.New("folder", "Listing .../b/c", "Listed .../b/c", "Listing directory attempted")},
		{"WebSearch", `{"query":"flatbuffers swift"}`,
			toollabel.New("globe", "Searching flatbuffers swift", "Searched flatbuffers swift", "Web search attempted")},
		{"WebFetch", `{"url":"https://go.dev/doc"}`,
			toollabel.New("arrow.down.circle", "Fetching https://go.dev/doc", "Fetched https://go.dev/doc", "Fetch attempted")},
		{"Agent", `{"description":"Explore the relay","subagent_type":"Explore"}`,
			toollabel.New("person.2", "Running Explore the relay", "Ran Explore the relay", "Subagent failed: Explore the relay")},
		{"Task", `{"prompt":"Find the bug"}`,
			toollabel.New("person.2", "Running Find the bug", "Ran Find the bug", "Subagent failed: Find the bug")},
		{"TaskCreate", `{"subject":"Port labels"}`,
			toollabel.New("checklist", "Creating task: Port labels", "Created task", "Task creation attempted")},
		{"TaskCreate", `{}`,
			toollabel.New("checklist", "Creating task: task", "Created task", "Task creation attempted")},
		{"TaskUpdate", `{"taskId":"7","subject":"Write tests"}`,
			toollabel.New("checklist", "Updating task Write tests", "Updated task", "Task update attempted")},
		{"TaskUpdate", `{"taskId":"7"}`,
			toollabel.New("checklist", "Updating task #7", "Updated task", "Task update attempted")},
		{"TaskUpdate", `{}`,
			toollabel.New("checklist", "Updating task #task", "Updated task", "Task update attempted")},
		{"TodoWrite", `{"todos":[{},{}]}`,
			toollabel.New("checklist", "Updating 2 todos", "Updated 2 todos", "Updating todos attempted")},
		{"AskUserQuestion", `{"questions":[{"header":"Scope","question":"Which?"}]}`,
			toollabel.New("questionmark.bubble", "Scope", "Answered question", "Question attempted")},
		{"ToolSearch", `{"query":"select:WebFetch"}`,
			toollabel.New("wrench.and.screwdriver", "Enabling WebFetch", "Enabled WebFetch", "Tool enable attempted")},
		{"EnterPlanMode", `{}`,
			toollabel.New("list.bullet.clipboard", "Entering plan mode", "Planning the approach", "Plan mode attempted")},
		{"ExitPlanMode", `{"plan":"…"}`,
			toollabel.New("checkmark.circle", "Finishing the plan", "Presented plan for approval", "Plan attempted")},
		{"Skill", `{"skill":"swift-ui"}`,
			toollabel.New("arrow.down.doc", "Loading skill: swift-ui", "Loaded skill: swift-ui", "Skill load attempted")},
		{"Monitor", `{}`,
			toollabel.New("waveform.path.ecg", "Watching a background task", "Watched a background task", "Watching failed")},
		{"TaskOutput", `{}`,
			toollabel.New("text.alignleft", "Reading background output", "Read background output", "Reading output failed")},
		{"TaskStop", `{}`,
			toollabel.New("stop.circle", "Stopping a background task", "Stopped a background task", "Stopping failed")},
		{"SendMessage", `{"to":"reviewer"}`,
			toollabel.New("arrow.turn.down.right", "Messaging reviewer", "Messaged reviewer", "Messaging failed")},
		{"SendMessage", `{}`,
			toollabel.New("arrow.turn.down.right", "Messaging another agent", "Messaged another agent", "Messaging failed")},
		{"ListAgents", `{}`,
			toollabel.New("list.bullet", "Listing agents", "Listed agents", "Listing agents failed")},
		{"ScheduleWakeup", `{}`,
			toollabel.New("alarm", "Scheduling a wake-up", "Scheduled a wake-up", "Scheduling failed")},
		{"LSP", `{}`,
			toollabel.New("curlybraces", "Asking the language server", "Asked the language server", "Language server failed")},
		{"PushNotification", `{}`,
			toollabel.New("bell", "Sending a notification", "Sent a notification", "Notification failed")},
		{"mcp__repogo__memory", `{}`,
			toollabel.New("powerplug.fill", "Memory · repogo", "Memory · repogo", "Memory attempted")},
		{"NotebookEdit", `{}`,
			toollabel.New("wrench.and.screwdriver", "Running Notebook Edit", "Ran Notebook Edit", "Notebook Edit attempted")},
	} {
		got := toolLabel(tc.name, tc.input)
		if got != tc.want {
			t.Errorf("%s %s:\n got %+v\nwant %+v", tc.name, tc.input, got, tc.want)
		}
	}
}

// The chat lane's row: labels on a named call, plus what the chat reads
// without opening it.
func TestToolRow(t *testing.T) {
	p := &Provider{}
	row := p.ToolRow(agent.ToolCall{CallID: "c1", Name: "TodoWrite",
		Input:  json.RawMessage(`{"todos":[{"content":"Port labels","status":"in_progress","id":"1"}]}`),
		Output: "Todos have been modified successfully."})
	if row.Name != "TodoWrite" || row.Labels == nil || row.Labels.Completed != "Updated 1 todo" ||
		len(row.Todos) != 1 || row.Todos[0].Content != "Port labels" || row.Todos[0].Status != "in_progress" {
		t.Errorf("TodoWrite: %+v", row)
	}

	row = p.ToolRow(agent.ToolCall{Name: "Agent",
		Input: json.RawMessage(`{"description":"Audit the relay","subagent_type":"Explore","prompt":"…"}`)})
	if row.SubagentType != "Explore" || row.Labels.Active != "Running Audit the relay" || row.Background {
		t.Errorf("Agent: %+v", row)
	}

	row = p.ToolRow(agent.ToolCall{Name: "Edit", Input: json.RawMessage(`{"file_path":"/repo/Chat.swift"}`)})
	if row.File != "Chat.swift" || row.Icon != "square.and.pencil" {
		t.Errorf("Edit: %+v", row)
	}

	// A result row names no tool; a background launch is read from its output.
	row = p.ToolRow(agent.ToolCall{CallID: "c2", Output: "Async agent launched successfully. agentId: …"})
	if !row.Background || row.Labels != nil || row.Name != "" {
		t.Errorf("result row: %+v", row)
	}
}
