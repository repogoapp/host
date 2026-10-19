package store

import (
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

func TestSummaryIsTheNewestTurnsRecap(t *testing.T) {
	prompt := agent.Event{Kind: agent.EventUserMessage, Text: "go"}
	tool := agent.Event{Kind: agent.EventToolCall, Tool: &agent.ToolCall{}}
	text := func(s string) agent.Event { return agent.Event{Kind: agent.EventText, Text: s} }
	done := agent.Event{Kind: agent.EventTurnFinished}
	cases := []struct {
		name   string
		events []agent.Event
		want   string
	}{
		{"Claude's blocks joined", []agent.Event{prompt, text("Working."), tool, text("Done."), text("> Renamed the column."), done}, "Renamed the column."},
		{"one Codex message", []agent.Event{prompt, text("Done.\n\n> Renamed it.\n\n```ui\n<List/>\n```"), done}, "Renamed it."},
		{"no recap takes the opening", []agent.Event{prompt, text("It's 42.")}, "It's 42."},
		{"a card alone doesn't revive the last turn", []agent.Event{prompt, text("> Old recap."), prompt, tool, text("```ui\n<List/>\n```")}, ""},
		{"a turn with no reply yet", []agent.Event{prompt, text("> Old recap."), prompt}, ""},
		{"text before a tool call isn't the final message", []agent.Event{prompt, text("> Planning."), tool}, ""},
	}
	for _, c := range cases {
		if got := summaryOf(c.events); got != c.want {
			t.Errorf("%s: summary = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestChatRowsCarryTheSummary(t *testing.T) {
	db := newStore(t)
	err := db.Sync(session.Meta{Agent: agent.KindClaude, ID: "a", Cwd: "/tmp/a", UpdatedAt: time.UnixMilli(100), SizeBytes: 1},
		[]agent.Event{{Kind: agent.EventUserMessage, Text: "rename it"}, {Kind: agent.EventText, Text: "Done.\n\n> Renamed the column."}})
	if err != nil {
		t.Fatal(err)
	}
	page, err := db.Chats(ChatQuery{Limit: 10})
	if err != nil || len(page.Chats) != 1 || page.Chats[0].Summary != "Renamed the column." {
		t.Fatalf("rows = %+v %v", page.Chats, err)
	}
}
