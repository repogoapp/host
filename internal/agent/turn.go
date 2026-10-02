package agent

import (
	"context"
	"sync"
	"time"
)

// subBuffer is generous because a stalled subscriber is dropped rather than
// allowed to block the turn; it can resubscribe and replay the whole turn.
const subBuffer = 512

type turn struct {
	req    TurnRequest
	ctx    context.Context
	cancel context.CancelFunc
	// Restored after a restart: advance skips it until Send Now. Guarded by
	// the Manager's mu, as req is while queued.
	held bool

	mu         sync.Mutex
	status     TurnStatus
	chatStatus ChatStatus
	onStatus   func(TurnStatus, ChatStatus, time.Time)
	seq        uint64
	// Full history, so replay is complete; freed when the Manager forgets the turn.
	events []Event
	subs   map[chan Event]struct{}

	// Last time anything was published, for the stall sweeper.
	lastEvent    time.Time
	stalled      bool
	providerBusy bool
}

func (t *turn) lastEventBefore(cutoff time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status.State == StateRunning && !t.providerBusy && t.status.Approval == nil && t.lastEvent.Before(cutoff)
}

func (t *turn) activity(busy bool) {
	t.mu.Lock()
	t.lastEvent = time.Now()
	t.providerBusy = busy
	t.mu.Unlock()
}

func (t *turn) snapshot() TurnStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.status
	s.EventCount = len(t.events)
	return s
}

func (t *turn) publish(e Event) {
	t.mu.Lock()
	changed, status, activity, at := t.appendLocked(e)
	t.mu.Unlock()
	if changed {
		t.onStatus(status, activity, at)
	}
}

// appendLocked records e and fans it out; t.mu must be held.
func (t *turn) appendLocked(e Event) (changed bool, status TurnStatus, activity ChatStatus, at time.Time) {
	t.seq++
	t.lastEvent = time.Now()
	e.Seq = t.seq
	e.TurnID = t.status.TurnID
	if e.At == 0 {
		e.At = t.lastEvent.UnixMilli()
	}
	t.events = append(t.events, e)
	activity = t.chatStatus
	switch e.Kind {
	case EventTurnStarted:
		activity = ChatWorking
	case EventApprovalResolved:
		activity = ChatWorking
		if e.Approval != nil && t.status.Approval != nil && t.status.Approval.CallID == e.Approval.CallID {
			t.status.Approval = nil
		}
	case EventApprovalRequested:
		t.status.Approval = e.Approval
		activity = ChatAwaitingApproval
		if e.Approval != nil && e.Approval.Kind == ApprovalQuestion {
			activity = ChatAwaitingUser
		}
	}
	changed = activity != t.chatStatus
	t.chatStatus = activity
	for ch := range t.subs {
		select {
		case ch <- e:
		default:
			delete(t.subs, ch)
			close(ch)
		}
	}
	return changed, t.status, activity, t.lastEvent
}

func (t *turn) settle(state TurnState, errMsg string, usage *Usage, sessionID string) {
	kind := EventTurnFinished
	if state == StateFailed {
		kind = EventTurnFailed
	}
	now := time.Now()
	t.mu.Lock()
	// A turn settles once: a second settle must neither publish another
	// terminal event nor overwrite the real outcome.
	if t.status.EndedAt != nil {
		t.mu.Unlock()
		return
	}
	t.appendLocked(Event{Kind: kind, Error: errMsg, Usage: usage, SessionID: sessionID})
	t.status.State = state
	t.status.Approval = nil
	switch state {
	case StateDone:
		t.chatStatus = ChatCompleted
	case StateFailed:
		t.chatStatus = ChatFailed
	case StateStopped:
		t.chatStatus = ChatCancelled
	}
	t.status.Error = errMsg
	t.status.EndedAt = &now
	if usage != nil {
		t.status.Usage = usage
	}
	if sessionID != "" {
		t.status.SessionID = sessionID
	}
	for ch := range t.subs {
		delete(t.subs, ch)
		close(ch)
	}
	status, activity := t.status, t.chatStatus
	t.mu.Unlock()
	t.onStatus(status, activity, now)
}

func (t *turn) subscribe() (<-chan Event, func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Size for the backlog plus headroom so replay cannot self-overflow.
	ch := make(chan Event, len(t.events)+subBuffer)
	for _, e := range t.events {
		ch <- e
	}

	if t.status.EndedAt != nil {
		close(ch)
		return ch, func() {}
	}

	t.subs[ch] = struct{}{}
	return ch, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if _, ok := t.subs[ch]; ok {
			delete(t.subs, ch)
			close(ch)
		}
	}
}
