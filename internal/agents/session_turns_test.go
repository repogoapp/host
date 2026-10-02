package agents_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/session"
)

func turnsOf(events []agent.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.TurnID
	}
	return out
}

func call(id string) *agent.ToolCall { return &agent.ToolCall{CallID: id, Name: "Bash"} }

func TestClaudeParseStampsThePromptID(t *testing.T) {
	prompt, _ := json.Marshal(map[string]any{"type": "user", "promptId": "p1",
		"message": map[string]any{"role": "user", "content": "Fix the build"}})
	result, _ := json.Marshal(map[string]any{"type": "user", "promptId": "p1",
		"message": map[string]any{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}}}})
	reply, _ := json.Marshal(map[string]any{"type": "assistant",
		"message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "Done."}}}})

	c := &claude.Sessions{}
	var got []agent.Event
	for _, line := range [][]byte{prompt, result, reply} {
		got = append(got, session.Parse(c, line)...)
	}
	if want := []string{"p1", "p1", ""}; !slices.Equal(turnsOf(got), want) {
		t.Fatalf("parsed turns = %q, want %q", turnsOf(got), want)
	}
}

func TestClaudeTurnsFollowThePrompt(t *testing.T) {
	events := []agent.Event{
		{Kind: agent.EventUserMessage, TurnID: "p1"},
		{Kind: agent.EventReasoning},
		{Kind: agent.EventToolCall, Tool: call("a")},
		{Kind: agent.EventToolResult, TurnID: "p1", Tool: call("a")},
		// Typed while p1 ran: delivered into it, with no id of its own.
		{Kind: agent.EventUserMessage},
		{Kind: agent.EventText},
		{Kind: agent.EventUserMessage, TurnID: "p2"},
		{Kind: agent.EventTurnFailed, Error: "overloaded"},
	}
	(&claude.Sessions{}).Normalize(events)
	want := []string{"p1", "p1", "p1", "p1", "p1", "p1", "p2", "p2"}
	if got := turnsOf(events); !slices.Equal(got, want) {
		t.Fatalf("turns = %q, want %q", got, want)
	}
}

// A transcript from before promptId names no turn, rather than one made up.
func TestClaudeTurnsStayEmptyWithoutIDs(t *testing.T) {
	events := []agent.Event{{Kind: agent.EventUserMessage}, {Kind: agent.EventText}}
	(&claude.Sessions{}).Normalize(events)
	if got := turnsOf(events); !slices.Equal(got, []string{"", ""}) {
		t.Fatalf("turns = %q, want none", got)
	}
}

func TestCodexItemsTakeTheOpenTurn(t *testing.T) {
	events := []agent.Event{
		{Kind: agent.EventTurnStarted, TurnID: "a"},
		{Kind: agent.EventUserMessage},
		{Kind: agent.EventToolCall, Tool: call("1")},
		{Kind: agent.EventToolResult, Tool: call("1")},
		{Kind: agent.EventTurnFinished, TurnID: "a"},
		// Between turns: nothing settles it.
		{Kind: agent.EventText},
		{Kind: agent.EventTurnStarted, TurnID: "b"},
		{Kind: agent.EventText},
	}
	(&codex.Sessions{}).Normalize(events)
	want := []string{"a", "a", "a", "a", "a", "", "b", "b"}
	if got := turnsOf(events); !slices.Equal(got, want) {
		t.Fatalf("turns = %q, want %q", got, want)
	}
}

// Seen in real rollouts: a prompt sent while a turn runs starts a second turn,
// and the first keeps writing until its own end line.
func TestCodexOverlappingTurns(t *testing.T) {
	events := []agent.Event{
		{Kind: agent.EventTurnStarted, TurnID: "a"},
		{Kind: agent.EventUserMessage},
		{Kind: agent.EventToolCall, Tool: call("1")},
		{Kind: agent.EventTurnStarted, TurnID: "b"},
		{Kind: agent.EventUserMessage},                 // b's prompt
		{Kind: agent.EventReasoning},                   // either turn's
		{Kind: agent.EventToolResult, Tool: call("1")}, // a's call
		{Kind: agent.EventToolCall, Tool: call("2")},   // either turn's
		{Kind: agent.EventTurnFinished, TurnID: "a"},   // 8
		{Kind: agent.EventText},                        // only b is open
		{Kind: agent.EventTurnFinished, TurnID: "b"},
	}
	(&codex.Sessions{}).Normalize(events)
	want := []string{"a", "a", "a", "b", "b", "", "a", "", "a", "b", "b"}
	if got := turnsOf(events); !slices.Equal(got, want) {
		t.Fatalf("turns = %q, want %q", got, want)
	}
}

// A turn with no end line (the CLI was killed) ends where the next starts, so
// it does not make every later row ambiguous.
func TestCodexAbandonedTurnEndsAtTheNext(t *testing.T) {
	events := []agent.Event{
		{Kind: agent.EventTurnStarted, TurnID: "a"},
		{Kind: agent.EventTurnStarted, TurnID: "b"},
		{Kind: agent.EventUserMessage},
		{Kind: agent.EventText},
	}
	(&codex.Sessions{}).Normalize(events)
	want := []string{"a", "b", "b", "b"}
	if got := turnsOf(events); !slices.Equal(got, want) {
		t.Fatalf("turns = %q, want %q", got, want)
	}
}
