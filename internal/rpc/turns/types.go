package turns

import (
	"encoding/json"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agentusage"
)

type UsageResult struct {
	Agents []agentusage.Usage `json:"agents" wire:"array"`
}

// ResetParams' credit_id may be empty, to let Codex pick one.
type ResetParams struct {
	Kind     string `json:"kind"`
	CreditID string `json:"credit_id"`
}

type ResetResult struct {
	Outcome string `json:"outcome"`
}

type TurnParams struct {
	TurnID string `json:"turn_id"`
}

// StopParams stops a running turn, or drops one still queued.
type StopParams struct {
	TurnID string `json:"turn_id"`
}

// EditQueuedParams replaces a queued turn's prompt while it still waits; a
// turn that has started is refused, never re-run.
type EditQueuedParams struct {
	TurnID string `json:"turn_id"`
	Prompt string `json:"prompt"`
}

// RemoveQueuedParams drops a turn only while it still waits, so a queue row
// acted on late cannot stop the turn it has become.
type RemoveQueuedParams struct {
	TurnID string `json:"turn_id"`
}

type TurnsResult struct {
	Turns []agent.TurnStatus `json:"turns"`
}

type RespondParams struct {
	TurnID string          `json:"turn_id"`
	CallID string          `json:"call_id"`
	Answer json.RawMessage `json:"answer"`
}
