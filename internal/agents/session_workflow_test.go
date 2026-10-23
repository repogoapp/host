package agents_test

import (
	"path/filepath"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agents/claude"
	"github.com/repogoapp/host/internal/session"
)

// A Workflow call's launch result names its run; the run's journal lists its
// agents as they start and finish. Each agent is a child of the call.
func TestClaudeWorkflowAgentsAreChildrenOfTheirCall(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "s1.jsonl")
	jsonl(t, main,
		map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_use", "id": "toolu_wf", "name": "Workflow", "input": map[string]any{"script": "export const meta = { name: 'tiny' }"}},
		}}},
		// A prompt quoting the call id and a runId is not its launch result.
		claudeText("user", `what did toolu_wf return? "runId" was wf_other`, false),
		map[string]any{"type": "user", "toolUseResult": map[string]any{"status": "async_launched", "runId": "wf_1"},
			"message": map[string]any{"content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_wf", "content": "Workflow launched in background."},
			}}},
	)
	run := filepath.Join(dir, "s1", "subagents", "workflows", "wf_1")
	jsonl(t, filepath.Join(run, "journal.jsonl"),
		map[string]any{"type": "launched"},
		map[string]any{"type": "started", "agentId": "a1", "label": "map:math", "phase": "Map"},
		map[string]any{"type": "started", "agentId": "a2", "label": "map:color", "phase": "Map"},
		map[string]any{"type": "result", "agentId": "a1", "result": "2 + 2 is 4."},
		map[string]any{"type": "result", "agentId": "a2", "result": map[string]any{"color": "red"}},
		map[string]any{"type": "started", "agentId": "a3", "label": "build:summary", "phase": "Build"},
		map[string]any{"type": "started", "agentId": "../a4", "label": "escapes", "phase": "Build"},
	)
	jsonl(t, filepath.Join(run, "agent-a3.jsonl"),
		claudeText("user", "Combine them.", true),
		claudeText("assistant", "Working on it.", true),
	)

	c := &claude.Sessions{}
	meta := session.Meta{ID: "s1", Path: main}
	children, err := c.Children(meta)
	if err != nil {
		t.Fatal(err)
	}
	want := []session.Child{
		{Key: "a1", Label: "map:math", Phase: "Map", Order: 0, Done: true, Result: "2 + 2 is 4."},
		{Key: "a2", Label: "map:color", Phase: "Map", Order: 1, Done: true, Result: `{"color":"red"}`},
		{Key: "a3", Label: "build:summary", Phase: "Build", Order: 2},
	}
	if len(children) != len(want) {
		t.Fatalf("children = %+v, want %+v", children, want)
	}
	for i, w := range want {
		got := children[i]
		if got.Key != w.Key || got.CallID != "toolu_wf" || got.RunID != "wf_1" || got.Label != w.Label ||
			got.Phase != w.Phase || got.Order != w.Order || got.Done != w.Done || got.Result != w.Result {
			t.Errorf("child %d = %+v, want %+v of toolu_wf in wf_1", i, got, w)
		}
	}
	if len(children[0].Files) != 0 || len(children[2].Files) != 1 {
		t.Errorf("files = %v, %v; want none before an agent writes, then its transcript", children[0].Files, children[2].Files)
	}

	events, err := c.ChildEvents(meta, children[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Kind != agent.EventText || events[1].Text != "Working on it." {
		t.Errorf("a3 events = %+v, want its prompt and reply", events)
	}
}
