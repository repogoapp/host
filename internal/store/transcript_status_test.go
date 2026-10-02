package store

import (
	"testing"

	"github.com/repogo/host/internal/agent"
)

// Claude writes an API error as the turn's failure but no lifecycle for the
// turns after it; a later prompt means the failure is history, not the state.
func TestTranscriptStatusForgetsAFailureOnceTheUserCarriesOn(t *testing.T) {
	failed := []agent.Event{
		{Kind: agent.EventUserMessage, At: 1},
		{Kind: agent.EventTurnFailed, Error: "API Error: 529 Overloaded.", At: 2},
	}
	if status, at := transcriptStatus(failed); status != agent.ChatFailed || at != 2 {
		t.Fatalf("after the error: %v at %d, want failed at 2", status, at)
	}
	carriedOn := append(failed, agent.Event{Kind: agent.EventUserMessage, At: 3}, agent.Event{Kind: agent.EventText, At: 4})
	if status, _ := transcriptStatus(carriedOn); status != agent.ChatUnknown {
		t.Fatalf("after a new prompt: %v, want unknown", status)
	}

	// Codex: a new turn starts before its prompt and keeps its own state.
	codex := []agent.Event{
		{Kind: agent.EventTurnFailed, Error: "aborted", At: 1},
		{Kind: agent.EventTurnStarted, At: 2},
		{Kind: agent.EventUserMessage, At: 2},
	}
	if status, at := transcriptStatus(codex); status != agent.ChatWorking || at != 2 {
		t.Fatalf("codex: %v at %d, want working at 2", status, at)
	}
}
