package notify

import (
	"encoding/json"
	"time"

	"github.com/repogo/host/internal/agent"
)

// DisplayFrame is one batch of a message Claude renders in the user's terminal,
// from the MessageDisplay hook: the only view of text before the transcript has
// it. Frames bypass the Notice bus: they carry no status and arrive too often.
type DisplayFrame struct {
	Agent     agent.Kind
	SessionID string
	Cwd       string
	Origin    Origin

	// PromptID is what turn history keys the turn by; TurnID is Claude's own.
	PromptID  string
	TurnID    string
	MessageID string

	// Index orders batches within a message; Final marks its last one.
	Index int
	Final bool
	Delta string
	At    time.Time
}

// displayEvent is the hook whose drops are DisplayFrames.
const displayEvent = "MessageDisplay"

// parseDisplay is the frame a MessageDisplay drop carries, if usable. A frame
// without a session or message id cannot be attached to anything.
func parseDisplay(d drop, providers []Provider) (DisplayFrame, bool) {
	var p struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		PromptID  string `json:"prompt_id"`
		TurnID    string `json:"turn_id"`
		MessageID string `json:"message_id"`
		Index     int    `json:"index"`
		Final     bool   `json:"final"`
		Delta     string `json:"delta"`
	}
	if json.Unmarshal(d.Payload, &p) != nil || p.SessionID == "" || p.MessageID == "" {
		return DisplayFrame{}, false
	}
	kind := agent.Kind(d.Agent)
	if provider := find(providers, kind); provider == nil || !provider.Displays() {
		return DisplayFrame{}, false
	}
	return DisplayFrame{
		Agent: kind, SessionID: p.SessionID, Cwd: p.Cwd, Origin: d.origin(),
		PromptID: p.PromptID, TurnID: p.TurnID, MessageID: p.MessageID,
		Index: p.Index, Final: p.Final, Delta: p.Delta, At: d.time(),
	}, true
}
