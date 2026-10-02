package chatlive

import (
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/store"
)

// Token-level streaming of a turn the host is running; the file tail only sees
// finished messages. Snapshots, not deltas: each push carries the whole message
// so far, so duplicates and missed pushes self-heal.

const (
	// Fast enough to read as typing, slow enough that a phone is not repainting
	// on every token.
	streamInterval = 150 * time.Millisecond

	// A runaway turn must not stream forever into a client's memory.
	maxStreamedChars = 200_000
)

// TurnStream is the slice of the agent Manager this package needs.
type TurnStream interface {
	Subscribe(turnID string) (<-chan agent.Event, func(), error)
}

// StreamTurn relays an in-flight turn's text to everyone watching this chat.
// Turns started in the user's own terminal still arrive whole on the file tail.
func (m *Manager) StreamTurn(chatID store.ChatID, turnID string) {
	events, cancel, err := m.turns.Subscribe(turnID)
	if err != nil {
		m.log.Debug("chatlive: cannot stream turn", "turn", turnID, "err", err)
		return
	}
	m.mu.Lock()
	m.streaming[chatID]++
	m.mu.Unlock()
	go m.relay(chatID, turnID, events, cancel)
}

func (m *Manager) relay(chatID store.ChatID, turnID string, events <-chan agent.Event, cancel func()) {
	defer cancel()
	defer func() {
		m.mu.Lock()
		if m.streaming[chatID]--; m.streaming[chatID] == 0 {
			delete(m.streaming, chatID)
		}
		m.mu.Unlock()
	}()

	ticker := time.NewTicker(streamInterval)
	defer ticker.Stop()
	st := &streamState{m: m, chatID: chatID, turnID: turnID, leading: true}
	for {
		select {
		case <-m.ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				st.push(true)
				return
			}
			if st.apply(event) {
				return
			}
		case <-ticker.C:
			if st.dirty {
				st.push(false)
			}
		}
	}
}

// streamState is one relayed turn's snapshot so far.
type streamState struct {
	m      *Manager
	chatID store.ChatID
	turnID string

	text  strings.Builder
	dirty bool

	// Prose resuming after a tool call is a new paragraph; the deltas carry
	// no message boundary, and the file tail saves them as separate blocks.
	breakPending bool

	// The first token tells the user anything is happening, so it does not
	// wait for the timer.
	leading bool

	// Stamped from the events, since Subscribe replays from the start.
	timing streamTiming
}

func (st *streamState) push(done bool) {
	st.m.pushStream(st.chatID, st.turnID, st.text.String(), done, st.timing)
	st.dirty = false
}

// apply folds one event into the snapshot, pushing at once what must not wait
// for the timer, and reports whether the turn ended.
func (st *streamState) apply(event agent.Event) bool {
	m, chatID, turnID := st.m, st.chatID, st.turnID
	switch event.Kind {
	case agent.EventTurnStarted:
		st.timing.StartedAt = event.At
		// Nothing to show yet, but the device that just started the turn needs
		// something to stop.
		st.push(false)

	case agent.EventText:
		if st.breakPending && st.text.Len() > 0 && !strings.HasSuffix(st.text.String(), "\n") {
			st.text.WriteString("\n\n")
		}
		st.breakPending = false
		if st.text.Len() < maxStreamedChars {
			st.text.WriteString(event.Text)
		}
		st.dirty = true
		if st.leading {
			st.leading = false
			st.timing.noteFirstFrame(event.At)
			st.push(false)
		}

	case agent.EventReasoning, agent.EventToolCall:
		// Thinking and tool use are the agent working: the first one flips the
		// client from "Waiting" to "Working" at once.
		st.breakPending = true
		if st.timing.noteFirstFrame(event.At) {
			st.push(false)
		}

	case agent.EventApprovalRequested:
		// A delayed approval is indistinguishable from a hung agent.
		m.pushApproval(chatID, turnID, event.Approval)
		m.attend(Attention{ChatID: string(chatID), TurnID: turnID, Kind: AttentionApproval,
			Approval: event.Approval, Answerable: true})

	case agent.EventApprovalResolved:
		// Answered here or elsewhere; every other device drops its copy.
		m.pushApproval(chatID, turnID, nil)
		if event.Approval != nil {
			m.attend(Attention{ChatID: string(chatID), TurnID: turnID, Kind: AttentionResolved,
				CallID: event.Approval.CallID, Answerable: true})
		}

	case agent.EventTurnFinished, agent.EventTurnFailed:
		// One last snapshot so the final words do not wait for the file tail.
		m.pushApproval(chatID, turnID, nil)
		st.timing.EndedAt = event.At
		st.timing.StopReason = stopReason(event)
		st.push(true)
		m.attend(ended(chatID, turnID, st.timing.StopReason, event.Error, st.text.String(), true))
		return true
	}
	return false
}

// stopReason names how a turn ended in the client's terms: nothing for a clean
// finish, "stopped" for a cancel, "failed" for anything else.
func stopReason(event agent.Event) string {
	switch {
	case event.Error == "stopped":
		// A stop settles as a finished turn carrying this text, not a failed one.
		return "stopped"
	case event.Kind == agent.EventTurnFailed:
		return "failed"
	default:
		return ""
	}
}

// fullEvery bounds how long a device that missed a push shows stale text:
// between whole snapshots, pushes carry only what the text gained.
const fullEvery = 5 * time.Second

// streamSent is what a chat's last push left its watchers holding.
type streamSent struct {
	turnID string
	bytes  int
	fullAt time.Time
}

// pushStream sends a chat's text to its watchers and keeps it whole as the
// room's state. It only grows within a turn, so a push sends what it gained;
// a turn's first and last push, and one every fullEvery, send all of it.
func (m *Manager) pushStream(chatID store.ChatID, turnID, text string, done bool, timing streamTiming) {
	m.streamMu.Lock()
	defer m.streamMu.Unlock()
	now := m.now()
	prev, ok := m.streamSent[chatID]
	snapshot := Streaming{
		ChatID:       string(chatID),
		TurnID:       turnID,
		Text:         text,
		Done:         done,
		streamTiming: timing,
	}
	if ok && prev.turnID == turnID && !done && len(text) >= prev.bytes && now.Sub(prev.fullAt) < fullEvery {
		snapshot.delta, snapshot.from = true, prev.bytes
	} else {
		prev.fullAt = now
	}
	sent := m.emit.Publish(chatRoom(chatID), snapshot, func(cur emit.Event, ok bool) emit.Verdict {
		// A closing snapshot for another turn than the room's must not clear
		// the one that is.
		if done && ok && cur.(Streaming).TurnID != turnID {
			return emit.Pass
		}
		return emit.Keep
	})
	switch {
	case done:
		delete(m.streamSent, chatID)
	case sent:
		m.streamSent[chatID] = streamSent{turnID: turnID, bytes: len(text), fullAt: prev.fullAt}
	}
}

// pushApproval forwards a permission prompt, or withdraws it with nil. It is
// room state until answered: the answering device usually opened the chat
// after the notification fired.
func (m *Manager) pushApproval(chatID store.ChatID, turnID string, approval *agent.Approval) {
	prompt := Approval{
		ChatID:   string(chatID),
		TurnID:   turnID,
		Approval: approval,
	}
	sent := m.emit.Publish(chatRoom(chatID), prompt, func(cur emit.Event, ok bool) emit.Verdict {
		// Nothing of this turn's to withdraw; a stale clear must not reach
		// devices that were never shown a prompt.
		if approval == nil && (!ok || cur.(Approval).TurnID != turnID) {
			return emit.Drop
		}
		return emit.Keep
	})
	if sent {
		m.onApproval(string(chatID), turnID, approval)
	}
}

// streamTiming is the turn's clock in Unix milliseconds, host time throughout
// so the settled duration is server-vs-server and a phone's drift only moves
// the live count. Zero is "not yet".
type streamTiming struct {
	// The Manager started running the turn.
	StartedAt int64 `json:"started_at,omitempty"`

	// The agent first did something — text, thinking, or a tool call. Until
	// this the client shows "Waiting on agent"; from it, "Working for".
	FirstFrameAt int64 `json:"first_frame_at,omitempty"`

	EndedAt int64 `json:"ended_at,omitempty"`

	// "stopped" or "failed"; empty for a clean finish. Only set with Done.
	StopReason string `json:"stop_reason,omitempty"`
}

// noteFirstFrame records the first sign of work. Reports whether this was it.
func (t *streamTiming) noteFirstFrame(at int64) bool {
	if t.FirstFrameAt != 0 {
		return false
	}
	if at == 0 {
		at = time.Now().UnixMilli()
	}
	t.FirstFrameAt = at
	return true
}
