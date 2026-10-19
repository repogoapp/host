package turns

import (
	"encoding/json"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentusage"
)

type UsageResult struct {
	Agents []agentusage.Usage `json:"agents" wire:"array"`
}

// ResetParams' credit_id may be empty, to let Codex pick one.
type ResetParams struct {
	Agent    string `json:"agent"`
	CreditID string `json:"credit_id"`
}

type ResetResult struct {
	Outcome string `json:"outcome"`
}

// TurnParams names the turn turns.get, turns.stop, turns.remove_queued and
// turns.send_queued act on.
type TurnParams struct {
	TurnID string `json:"turn_id"`
}

// Turn is a turn as the phone follows it: whether it opened its chat
// (SessionID set), how it ended, and what it waits on.
type Turn struct {
	TurnID    string          `json:"turn_id"`
	ChatID    string          `json:"chat_id"`
	State     agent.TurnState `json:"state"`
	SessionID string          `json:"session_id,omitempty"`
	Error     string          `json:"error,omitempty"`

	// The prompt the turn is blocked on, so a device that arrives late (voice
	// starting) learns it from turns.list without having watched the chat.
	Approval *agent.Approval `json:"approval,omitempty"`
}

func turnOf(s agent.TurnStatus) Turn {
	return Turn{TurnID: s.TurnID, ChatID: s.ChatID, State: s.State, SessionID: s.SessionID, Error: s.Error, Approval: s.Approval}
}

// EditQueuedParams replaces a queued turn's prompt while it still waits; a
// turn that has started is refused, never re-run.
type EditQueuedParams struct {
	TurnID string `json:"turn_id"`
	Prompt string `json:"prompt"`
}

type TurnsResult struct {
	Turns []Turn `json:"turns"`
}

type RespondParams struct {
	TurnID string          `json:"turn_id"`
	CallID string          `json:"call_id"`
	Answer json.RawMessage `json:"answer"`
}
