package chatlive

import (
	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/chatwire"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/store"
)

// Every event the chats family emits, in one place. The client declares the
// same set under the same names in ChatEvents.swift; the emit catalog test
// holds the two together.

func init() {
	emit.Register(Appended{}, Tool{}, Streaming{}, Approval{}, Changed{}, Removed{}, Attention{})
}

// chatRoom is the room for one chat's watchers.
func chatRoom(id store.ChatID) emit.Room { return emit.Room("chat:" + string(id)) }

// Appended is new transcript rows from a device's cursor on, per device, in
// a chats.messages reply's shape so a push and a fetched page share one
// client path.
type Appended struct {
	chatwire.Page
}

func (Appended) Method() string { return "chats.appended" }

// Tool is one call a device's open tool sheet shows, sent whole each time a
// row of it lands, so a running command's sheet fills in without a refetch.
type Tool struct {
	ChatID string `json:"chat_id"`
	chatwire.ToolDetail
}

func (Tool) Method() string { return "chats.tool" }

// Streaming is the whole text of an in-flight turn so far, not the piece
// since last time, so duplicates and missed pushes self-heal. The last one
// is the room's state until the turn is done.
type Streaming struct {
	ChatID string `json:"chat_id"`
	TurnID string `json:"turn_id"`
	Text   string `json:"text"`

	// Offset is where Text goes in the turn's text, in bytes, on a push that
	// carries only what was added; without it, Text is the whole of it.
	Offset *int `json:"offset,omitempty"`

	// The turn is over. The client keeps showing this text until the real,
	// indexed event arrives on the append lane, so the message does not blink
	// out and back in as it settles.
	Done bool `json:"done"`

	streamTiming
	emit.Stamp

	// delta sends Text from `from` on (emit.Delta); the room keeps it whole.
	delta bool
	from  int
}

func (Streaming) Method() string { return "chats.streaming" }

// Delta is the push: what the text gained since the last one, or all of it.
func (s Streaming) Delta() emit.Event {
	if !s.delta {
		return s
	}
	from := s.from
	s.Text, s.Offset = s.Text[from:], &from
	return s
}
func (Streaming) StateKey() string { return "streaming" }
func (s Streaming) Retain() bool   { return !s.Done }
func (s Streaming) Stamped(st emit.Stamp) emit.Stateful {
	s.Stamp = st
	return s
}

// Approval is the prompt a turn is blocked on: its own method, since ignoring
// it leaves the agent blocked. A nil approval withdraws it, answered here or
// elsewhere.
type Approval struct {
	ChatID   string          `json:"chat_id"`
	TurnID   string          `json:"turn_id"`
	Approval *agent.Approval `json:"approval"`
	emit.Stamp
}

func (Approval) Method() string   { return "chats.approval" }
func (Approval) StateKey() string { return "approval" }
func (a Approval) Retain() bool   { return a.Approval != nil }
func (a Approval) Stamped(st emit.Stamp) emit.Stateful {
	a.Stamp = st
	return a
}

// Live is the chats.subscribe reply: the turn streaming and the prompt it is
// blocked on, null when none, so a returning device clears what ended while
// it was away. Pushes at or below Revision are already reflected.
type Live struct {
	emit.Stamp
	Streaming *Streaming `json:"streaming"`
	Approval  *Approval  `json:"approval"`
}

// Changed is one chat's list row after it moved: a sweep found new activity,
// it was resolved, a send landed in it. Sent to every paired device, since a
// list is open on any of them; same shape as a chats.list row.
type Changed struct {
	store.Chat
}

func (Changed) Method() string { return "chats.changed" }

// Removed is a chat the user deleted, transcript and all. Sent to every
// paired device so lists and an open transcript let go of it.
type Removed struct {
	ChatID string `json:"chat_id"`
}

func (Removed) Method() string { return "chats.removed" }

// Attention kinds: a turn is blocked on the user, was unblocked, or ended.
const (
	AttentionApproval = "approval"
	AttentionResolved = "resolved"
	AttentionFinished = "finished"
	AttentionFailed   = "failed"
	AttentionStopped  = "stopped"
)

// Attention is something about a chat that wants the user, sent to every
// paired device rather than the chat's room, so the voice agent can say and
// answer it whichever chat is open. A question is an approval with questions.
type Attention struct {
	ChatID   string          `json:"chat_id"`
	TurnID   string          `json:"turn_id"`
	Kind     string          `json:"kind"`
	Approval *agent.Approval `json:"approval,omitempty"`
	// The call a resolved approval answered.
	CallID string `json:"call_id,omitempty"`
	Error  string `json:"error,omitempty"`
	// The turn's final text, clipped to maxAttentionReply.
	Reply string `json:"reply,omitempty"`
	// False for a turn in the user's terminal: turns.respond cannot reach it.
	Answerable bool `json:"answerable"`
}

func (Attention) Method() string { return "chats.attention" }
