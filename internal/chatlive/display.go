package chatlive

import (
	"cmp"
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/store"
)

// Streaming for turns run in the user's own terminal: tokens come from the
// MessageDisplay hook, not the turn relay, but leave on the same
// chats.streaming push so the client cannot tell the two apart.

// displayIdle is how long a turn may go without a frame before its snapshot
// is abandoned. Stop normally closes it; this covers a Claude that died.
const displayIdle = 10 * time.Minute

// displayTurn is one terminal turn's text so far.
type displayTurn struct {
	turnID string

	// Frames within a message arrive by index, but the drop directory sorts
	// by second and pid, so two in one second can swap. Parts holds what has
	// not yet joined the contiguous prefix.
	messageID string
	next      int
	parts     map[int]string

	text   strings.Builder
	lastAt time.Time

	// The turn's clock as the hooks saw it: UserPromptSubmit starts it, the
	// first tool call or text frame is the first sign of work, Stop ends it.
	// A turn first seen mid-flight has its first frame stand in for the start.
	timing streamTiming
}

// Display folds one MessageDisplay frame into its chat's snapshot and pushes
// the result to whoever is watching. The bridge calls this from its poll loop.
func (m *Manager) Display(f notify.DisplayFrame) {
	// A device's prompt streams over the turn relay under the Manager's turn id;
	// its hooks, polled late, would open a second turn with the same text.
	if f.Origin == notify.OriginApp {
		return
	}
	chatID := store.ChatID(agent.ChatID(f.Agent, f.SessionID))
	turnID := cmp.Or(f.PromptID, f.TurnID)

	m.displayMu.Lock()
	defer m.displayMu.Unlock()
	m.mu.Lock()
	t, ok := m.openDisplayLocked(chatID, turnID, f.At)
	if !ok {
		m.mu.Unlock()
		return
	}
	if t.messageID != f.MessageID {
		// A turn is several messages (text, tool calls, more text); the
		// snapshot is all of its text, the same as the turn relay's.
		t.messageID, t.next, t.parts = f.MessageID, 0, map[int]string{}
	}
	// A frame at or below the prefix is a replay of one already folded in.
	// Kept out of parts: an index below next would never be reached by the
	// drain below, and a part that can never drain is a loop that never ends.
	if f.Index >= t.next {
		t.parts[f.Index] = f.Delta
	}
	for {
		delta, ok := t.parts[t.next]
		if !ok {
			break
		}
		delete(t.parts, t.next)
		t.next++
		if t.text.Len() < maxStreamedChars {
			t.text.WriteString(delta)
		}
	}
	if f.Final && len(t.parts) > 0 {
		// The last frame landed but an earlier one never did (a killed hook):
		// take what is here in order; the transcript corrects it on append.
		idx := make([]int, 0, len(t.parts))
		for i := range t.parts {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for _, i := range idx {
			t.text.WriteString(t.parts[i])
		}
		t.parts = map[int]string{}
	}
	t.lastAt = f.At
	t.timing.noteFirstFrame(f.At.UnixMilli())
	timing := t.timing
	text := t.text.String()
	m.mu.Unlock()
	watchers := len(m.emit.Members(chatRoom(chatID)))

	m.log.Debug("chatlive: display frame", "chat", chatID, "turn", turnID,
		"message", f.MessageID, "index", f.Index, "final", f.Final,
		"chars", len(text), "watchers", watchers)
	m.pushStream(chatID, turnID, text, false, timing)
}

// openDisplayLocked returns the chat's terminal turn, closing a different open
// one first; false when the turn relay already streams this chat or the turn
// is stopped. Called with m.displayMu and m.mu held; may retake m.mu.
func (m *Manager) openDisplayLocked(chatID store.ChatID, turnID string, at time.Time) (*displayTurn, bool) {
	if m.streaming[chatID] > 0 || m.displayDone[chatID] == turnID {
		return nil, false
	}
	t := m.display[chatID]
	if t != nil && t.turnID != turnID {
		old := t
		m.displayDone[chatID] = old.turnID
		m.mu.Unlock()
		old.timing.EndedAt = at.UnixMilli()
		m.pushStream(chatID, old.turnID, old.text.String(), true, old.timing)
		m.mu.Lock()
		t = nil
	}
	if t == nil {
		t = &displayTurn{turnID: turnID, lastAt: at}
		t.timing.StartedAt = at.UnixMilli()
		m.display[chatID] = t
	}
	return t, true
}

// startDisplay opens a terminal turn on UserPromptSubmit and tells watchers
// at once: neither the file tail nor MessageDisplay says "a turn began", and
// a tool-heavy turn's first frame can be minutes in.
func (m *Manager) startDisplay(chatID store.ChatID, turnID string, at time.Time) {
	if turnID == "" {
		return
	}
	m.displayMu.Lock()
	defer m.displayMu.Unlock()
	m.mu.Lock()
	t, ok := m.openDisplayLocked(chatID, turnID, at)
	if !ok {
		m.mu.Unlock()
		return
	}
	timing, text := t.timing, t.text.String()
	m.mu.Unlock()
	m.pushStream(chatID, turnID, text, false, timing)
}

// noteDisplayWork marks a tool starting as the first sign of work, so the
// header reads "Working". With open it opens a missing turn: a tool about to
// run proves the turn is running; one that finished does not.
func (m *Manager) noteDisplayWork(chatID store.ChatID, turnID string, at time.Time, open bool) {
	m.displayMu.Lock()
	defer m.displayMu.Unlock()
	m.mu.Lock()
	t, ok := m.display[chatID]
	if !ok && open && turnID != "" {
		t, ok = m.openDisplayLocked(chatID, turnID, at)
	}
	if !ok || !t.timing.noteFirstFrame(at.UnixMilli()) {
		m.mu.Unlock()
		return
	}
	t.lastAt = at
	timing, turnID, text := t.timing, t.turnID, t.text.String()
	m.mu.Unlock()
	m.pushStream(chatID, turnID, text, false, timing)
}

// finishDisplay sends the closing snapshot for a terminal turn. `at` is the
// hook's own stamp, so durations are host time against host time. stopped
// (a Stop hook, not quiet) keeps the turn from reopening. Returns its text.
func (m *Manager) finishDisplay(chatID store.ChatID, at time.Time, stopReason string, stopped bool) string {
	m.displayMu.Lock()
	defer m.displayMu.Unlock()
	m.mu.Lock()
	t, ok := m.display[chatID]
	if ok {
		delete(m.display, chatID)
		if stopped {
			m.displayDone[chatID] = t.turnID
		}
	}
	m.mu.Unlock()
	if ok {
		t.timing.EndedAt = at.UnixMilli()
		t.timing.StopReason = stopReason
		m.pushStream(chatID, t.turnID, t.text.String(), true, t.timing)
		return t.text.String()
	}
	return ""
}

// hookStopReason names how a hook said a terminal turn ended, in the client's
// terms: nothing for a clean Stop, "stopped" for a cancel, "failed" otherwise.
func hookStopReason(n notify.Notice) string {
	switch {
	case n.Status == agent.ChatCancelled || n.Status == agent.ChatInterrupted:
		return "stopped"
	case n.Failed():
		return "failed"
	default:
		return ""
	}
}

// WatchNotices closes terminal turns on Stop and drops ones that went quiet.
// The recorder has already run by the time a notice reaches subscribers, so
// this never sees a Stop the turn history does not.
func (m *Manager) WatchNotices(ctx context.Context, bridge *notify.Bridge) {
	var sweeper sync.WaitGroup
	sweeper.Go(func() { m.sweepDisplays(ctx) })
	bridge.Follow(ctx, m.observeNotice)
	sweeper.Wait()
}

func (m *Manager) observeNotice(n notify.Notice) {
	chatID := store.ChatID(n.ChatID())
	_, turnID := n.Turn()
	reply := ""
	switch {
	case n.Origin == notify.OriginApp:
		// The turn relay streams and closes this turn; see Display.
	case n.Starts():
		m.startDisplay(chatID, turnID, n.At)
	case n.ToolStarting():
		m.noteDisplayWork(chatID, turnID, n.At, true)
	case n.ToolFinished():
		m.noteDisplayWork(chatID, turnID, n.At, false)
	case n.Ends():
		reply = m.finishDisplay(chatID, n.At, hookStopReason(n), true)
	}
	m.attendHook(n, reply)
	// The hook fired because the provider wrote something — a prompt, a tool
	// result, the final message. Send it to watchers now, not on the next poll.
	m.Nudge(chatID)
}

// sweepDisplays abandons terminal turns that went quiet until ctx ends.
func (m *Manager) sweepDisplays(ctx context.Context) {
	sweep := time.NewTicker(displayIdle / 2)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
		}
		var stale []store.ChatID
		m.mu.Lock()
		for id, t := range m.display {
			if time.Since(t.lastAt) > displayIdle {
				stale = append(stale, id)
			}
		}
		m.mu.Unlock()
		for _, id := range stale {
			// Quiet is not Stop: a turn that was only thinking may stream again.
			m.finishDisplay(id, time.Now(), "", false)
		}
	}
}
