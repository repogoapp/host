package chatlive

import (
	"cmp"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/store"
)

// maxAttentionReply is how much of a turn's final text rides chats.attention:
// v1's cap, enough for the voice agent to sum a turn up.
const maxAttentionReply = 600

func (m *Manager) attend(a Attention) { m.onAttention(a) }

// ended is the attention for a turn that is over, by its stop reason.
func ended(chatID store.ChatID, turnID, stopReason, errMsg, text string, answerable bool) Attention {
	a := Attention{ChatID: string(chatID), TurnID: turnID, Kind: AttentionFinished,
		Reply: clipReply(text), Answerable: answerable}
	switch stopReason {
	case "stopped":
		a.Kind = AttentionStopped
	case "failed":
		a.Kind, a.Error = AttentionFailed, errMsg
	}
	return a
}

// clipReply keeps the end of the text, where a turn says what it did.
func clipReply(text string) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= maxAttentionReply {
		return text
	}
	return "…" + strings.TrimSpace(string(runes[len(runes)-maxAttentionReply:]))
}

// attendHook turns a terminal turn's hook into attention. Nothing it sends is
// answerable: the prompt is in the user's terminal, out of turns.respond's
// reach. reply is the turn's streamed text when n ends it.
func (m *Manager) attendHook(n notify.Notice, reply string) {
	if n.Origin == notify.OriginApp {
		return
	}
	chatID := store.ChatID(n.ChatID())
	_, turnID := n.Turn()
	switch {
	case n.AsksPermission():
		m.setHookPending(chatID, n.ToolUseID)
		m.attend(Attention{ChatID: string(chatID), TurnID: turnID, Kind: AttentionApproval,
			Approval: &agent.Approval{CallID: n.ToolUseID, Title: cmp.Or(n.ToolName, n.Message, "Permission required"),
				Kind: "permission", Input: n.ToolInput}})

	case n.AsksQuestion():
		m.setHookPending(chatID, n.ToolUseID)
		m.attend(Attention{ChatID: string(chatID), TurnID: turnID, Kind: AttentionApproval,
			Approval: &agent.Approval{CallID: n.ToolUseID, Title: cmp.Or(n.Question(), "Question"),
				Kind: "question", Questions: n.Questions}})

	case n.ToolFinished():
		// The tool ran, so whatever was asked about it has been answered.
		if callID, ok := m.takeHookPending(chatID); ok {
			m.attend(Attention{ChatID: string(chatID), TurnID: turnID, Kind: AttentionResolved, CallID: callID})
		}

	case n.Ends():
		m.takeHookPending(chatID)
		m.attend(ended(chatID, turnID, hookStopReason(n), n.Error, reply, false))
	}
}

func (m *Manager) setHookPending(chatID store.ChatID, callID string) {
	m.mu.Lock()
	m.hookPending[chatID] = callID
	m.mu.Unlock()
}

func (m *Manager) takeHookPending(chatID store.ChatID) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	callID, ok := m.hookPending[chatID]
	delete(m.hookPending, chatID)
	return callID, ok
}
