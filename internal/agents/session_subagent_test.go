package agents_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/session"
)

func claudeText(role, text string, sidechain bool) map[string]any {
	return map[string]any{
		"type": role, "isSidechain": sidechain, "timestamp": "2026-09-24T10:00:00Z",
		"message": map[string]any{"role": role, "content": []map[string]any{{"type": "text", "text": text}}},
	}
}

// Older Claude Code wrote a subagent's conversation into the parent's file,
// marked isSidechain; it is the subagent's, not the chat's.
func TestClaudeSkipsSidechainLinesInTheMainFile(t *testing.T) {
	c := &claude.Sessions{}
	raw, _ := json.Marshal(claudeText("assistant", "the subagent talking", true))
	if got := session.Parse(c, raw); len(got) != 0 {
		t.Fatalf("main file: got %+v, want nothing", got)
	}
	sub := &claude.Sessions{Sidechain: true}
	if got := session.Parse(sub, raw); len(got) != 1 || got[0].Text != "the subagent talking" {
		t.Fatalf("subagent file: got %+v, want the text", got)
	}
}

// A background agent answers "launched" at once; its report arrives turns
// later in a task-notification, and belongs on the call that launched it.
func TestClaudeFoldsABackgroundAgentsReportOntoItsCall(t *testing.T) {
	notice := "<task-notification>\n<task-id>a1</task-id>\n<tool-use-id>toolu_task</tool-use-id>\n" +
		"<status>completed</status>\n<summary>Agent \"Audit\" finished</summary>\n" +
		"<result>Found two bugs.</result>\n<usage><tool_uses>3</tool_uses></usage>\n</task-notification>"
	lines := []map[string]any{
		{"type": "assistant", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_use", "id": "toolu_task", "name": "Agent", "input": map[string]any{"description": "Audit"}},
		}}},
		{"type": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "toolu_task", "content": "Async agent launched successfully."},
		}}},
		claudeText("assistant", "It is running.", false),
		{"type": "user", "origin": map[string]any{"kind": "task-notification"}, "message": map[string]any{"role": "user", "content": notice}},
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	items := make([]any, len(lines))
	for i, l := range lines {
		items[i] = l
	}
	jsonl(t, path, items...)

	events, err := session.ReadAll(session.Meta{ID: "s", Agent: agent.KindClaude, Path: path}, &claude.Sessions{})
	if err != nil {
		t.Fatal(err)
	}
	var results []*agent.ToolCall
	for _, e := range events {
		if e.Kind == agent.EventToolResult {
			results = append(results, e.Tool)
		}
		if e.Kind == agent.EventUserMessage {
			t.Errorf("the notice surfaced as a user message: %q", e.Text)
		}
	}
	if len(results) != 1 || results[0].CallID != "toolu_task" || results[0].Output != "Found two bugs." {
		t.Fatalf("results = %+v, want the report on the one Agent result", results)
	}
}

// A background shell command writes the same notice with no report; it is
// not a result.
func TestClaudeIgnoresANoticeWithoutAReport(t *testing.T) {
	notice := "<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>toolu_bash</tool-use-id>\n<status>completed</status>\n</task-notification>"
	raw, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": notice}})
	if got := session.Parse(&claude.Sessions{}, raw); len(got) != 0 {
		t.Fatalf("got %+v, want nothing", got)
	}
}

func TestClaudeSubagentReadsTheAgentsOwnTranscript(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "s1.jsonl")
	jsonl(t, main, claudeText("user", "look into it", false))
	subs := filepath.Join(dir, "s1", "subagents")
	jsonl(t, filepath.Join(subs, "agent-a9.jsonl"),
		claudeText("user", "Find the parser.", true),
		map[string]any{"type": "assistant", "isSidechain": true, "message": map[string]any{"content": []map[string]any{
			{"type": "tool_use", "id": "toolu_grep", "name": "Grep", "input": map[string]any{"pattern": "Parse"}},
		}}},
		claudeText("assistant", "It is in claude.go.", true),
	)
	if err := os.WriteFile(filepath.Join(subs, "agent-a9.meta.json"),
		[]byte(`{"agentType":"Explore","description":"Find the parser","toolUseId":"toolu_task"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	sub, err := (&claude.Sessions{}).Subagent(session.Meta{ID: "s1", Path: main}, "toolu_task")
	if err != nil {
		t.Fatal(err)
	}
	if sub.Kind != "Explore" {
		t.Errorf("kind = %q, want Explore", sub.Kind)
	}
	var kinds []agent.EventKind
	for _, e := range sub.Events {
		kinds = append(kinds, e.Kind)
	}
	want := []agent.EventKind{agent.EventUserMessage, agent.EventToolCall, agent.EventText}
	if len(kinds) != len(want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events = %v, want %v", kinds, want)
		}
	}

	if _, err := (&claude.Sessions{}).Subagent(session.Meta{ID: "s1", Path: main}, "toolu_other"); err != session.ErrNotFound {
		t.Errorf("unknown call: err = %v, want session.ErrNotFound", err)
	}
}

// Codex 0.14x writes a subagent's rollout under its parent's session_id, a
// copy of the parent's history first. It must neither list as a chat nor
// join its parent's; opened from the spawn call, it reads from its own start.
func TestCodexSubagentRollouts(t *testing.T) {
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "09", "24")

	parent := "019f-parent"
	jsonl(t, filepath.Join(day, "rollout-2026-09-24T10-00-00-"+parent+".jsonl"),
		map[string]any{"type": "session_meta", "payload": map[string]any{"session_id": parent, "id": parent, "cwd": "/w", "source": "cli"}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "audit this"}}}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "spawn_agent", "call_id": "call_spawn", "arguments": `{"task_name":"locate","message":"Find it"}`}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "call_spawn", "output": `{"task_name":"/root/locate"}`}},
	)
	jsonl(t, filepath.Join(day, "rollout-2026-09-24T10-00-05-019f-child.jsonl"),
		map[string]any{"type": "session_meta", "payload": map[string]any{
			"session_id": parent, "id": "019f-child", "cwd": "/w", "parent_thread_id": parent, "thread_source": "subagent",
			"agent_path": "/root/locate", "agent_nickname": "Laplace", "subagent_history_start_ordinal": 2,
			"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}},
		}},
		// The forked copy of the parent's history.
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "audit this"}}}},
		// The child's own work.
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "Found it in codex.go."}}}},
	)

	c := codex.NewSessions(home)
	metas, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].ID != parent || len(metas[0].Paths) != 1 {
		t.Fatalf("listed %+v, want only the parent, with its own rollout", metas)
	}

	sub, err := c.Subagent(metas[0], "call_spawn")
	if err != nil {
		t.Fatal(err)
	}
	if sub.Name != "Laplace" || sub.Kind != "locate" {
		t.Errorf("name, kind = %q, %q; want Laplace, locate", sub.Name, sub.Kind)
	}
	if len(sub.Events) != 1 || sub.Events[0].Text != "Found it in codex.go." {
		t.Fatalf("events = %+v, want only the child's own reply", sub.Events)
	}
}
