package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/errkind"
)

type TurnState string

const (
	StateQueued  TurnState = "queued"
	StateRunning TurnState = "running"
	StateDone    TurnState = "done"
	StateFailed  TurnState = "failed"
	StateStopped TurnState = "stopped"
)

type TurnStatus struct {
	TurnID string    `json:"turn_id"`
	ChatID string    `json:"chat_id"`
	Agent  Kind      `json:"agent"`
	Cwd    string    `json:"cwd"`
	Prompt string    `json:"prompt"`
	State  TurnState `json:"state"`

	// The model the device asked for; the transcript later says which ran.
	Model string `json:"model,omitempty"`
	// The permission the turn runs with. Off the wire: it is for the chat
	// row, as Claude records it only on a prompt line a long turn buries.
	PermissionMode PermissionMode `json:"-"`
	// The title a new chat was started with. Off the wire: it is for the chat
	// row and the agent's files, and no device reads it back from a turn.
	Title string `json:"-"`

	SessionID          string  `json:"session_id,omitempty"`
	ProviderStopReason *string `json:"provider_stop_reason,omitempty"`
	Error              string  `json:"error,omitempty"`
	Usage              *Usage  `json:"usage,omitempty"`

	QueuedAt  time.Time  `json:"queued_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`

	EventCount int `json:"event_count"`

	// The prompt the turn is blocked on, so a device that arrives late (voice
	// starting) learns it from turns.list without having watched the chat.
	Approval *Approval `json:"approval,omitempty"`
}

// StoredTurn is a turn waiting in its chat's queue as state.db keeps it:
// enough to run it after a restart.
type StoredTurn struct {
	TurnID   string
	QueuedAt time.Time
	// Restored after a restart: it waits for Send Now instead of running.
	Held    bool
	Request TurnRequest
}

// Manager owns turn lifecycle: queueing, sequencing, fan-out, and stop. Turns
// are serialized per ChatID and concurrent across chats, a product rule.
type Manager struct {
	log      *slog.Logger
	adapters map[Kind]Adapter
	hooks    Hooks

	updateUntil time.Time

	// Held across a queue change and its save, so saves land in the order the
	// changes were made. Taken before mu.
	queueMu sync.Mutex

	mu      sync.Mutex
	turns   map[string]*turn
	waiting map[string]chan json.RawMessage // "<turnID>\x00<callID>" -> the answer
	running map[string]*turn                // chat id -> the turn currently executing
	pending map[string][]*turn              // chat id -> FIFO of queued turns

	// Closed and replaced each time a chat's running turn ends, so StopAll
	// can wait for the set to empty.
	ended chan struct{}
}

// Hooks is who hears about turns; every one is required.
type Hooks struct {
	// Session is told when a turn learns which conversation it belongs to. One
	// hook for the whole Manager: a per-turn registry would be a second place to leak from.
	Session func(TurnStatus)
	// Status is told a chat's state as its running turn moves.
	Status func(TurnStatus, ChatStatus, time.Time)
	// SaveQueue keeps a chat's queue after every change to it (an enqueue, an
	// edit, a drop, a start, a rekey), so a restart restores it and devices
	// read its count from the chat's row.
	SaveQueue func(chatID string, queued []StoredTurn) error
}

func NewManager(log *slog.Logger, hooks Hooks, adapters ...Adapter) *Manager {
	m := &Manager{
		log:      log,
		hooks:    hooks,
		adapters: make(map[Kind]Adapter, len(adapters)),
		turns:    make(map[string]*turn),
		running:  make(map[string]*turn),
		pending:  make(map[string][]*turn),
		waiting:  make(map[string]chan json.RawMessage),
		ended:    make(chan struct{}),
	}
	for _, a := range adapters {
		m.adapters[a.Kind()] = a
	}
	return m
}

// queuedLocked is a chat's queue as state.db keeps it. Called with m.mu held.
func (m *Manager) queuedLocked(chatID string) []StoredTurn {
	q := m.pending[chatID]
	out := make([]StoredTurn, 0, len(q))
	for _, t := range q {
		out = append(out, StoredTurn{TurnID: t.status.TurnID, QueuedAt: t.status.QueuedAt, Held: t.held, Request: t.req})
	}
	return out
}

// saveQueue keeps a chat's queue as captured. Called with m.queueMu held and
// m.mu not. A failed save leaves the queue running from memory.
func (m *Manager) saveQueue(chatID string, queued []StoredTurn) {
	if err := m.hooks.SaveQueue(chatID, queued); err != nil {
		m.log.Error("saving the queue failed", "chat", chatID, "err", err)
	}
}

func (m *Manager) statusChanged(status TurnStatus, activity ChatStatus, at time.Time) {
	m.mu.Lock()
	if status.SessionID == "" {
		if turn := m.turns[status.TurnID]; turn != nil {
			status.SessionID = turn.req.SessionID
		}
	}
	active := m.running[status.ChatID]
	// Cancelling a queued turn must not replace the running turn's chat state.
	current := active == nil || active.status.TurnID == status.TurnID
	m.mu.Unlock()
	if current && status.SessionID != "" {
		m.hooks.Status(status, activity, at)
	}
}

// noteSession records the provider's conversation id on the status and tells
// the hook. A turn that opened the conversation is re-keyed to the provider's
// name for it, which is the id every later send will queue on.
func (m *Manager) noteSession(t *turn, sessionID string) {
	if sessionID == "" {
		return
	}
	t.mu.Lock()
	if t.status.SessionID == sessionID {
		t.mu.Unlock()
		return
	}
	t.status.SessionID = sessionID
	old := t.status.ChatID
	if t.req.SessionID == "" {
		t.status.ChatID = ChatID(t.req.Agent, sessionID)
	}
	status := t.status
	activity := t.chatStatus
	t.mu.Unlock()

	var rekeyed []TurnStatus
	m.queueMu.Lock()
	m.mu.Lock()
	moved := false
	var oldQueue, newQueue []StoredTurn
	if status.ChatID != old && m.running[old] == t {
		delete(m.running, old)
		m.running[status.ChatID] = t
		if q := m.pending[old]; len(q) > 0 {
			for _, queued := range q {
				// Behind the turn that opened the conversation, so they continue it.
				queued.req.ChatID, queued.req.SessionID = status.ChatID, sessionID
				queued.mu.Lock()
				queued.status.ChatID = status.ChatID
				rekeyed = append(rekeyed, queued.status)
				queued.mu.Unlock()
			}
			m.pending[status.ChatID] = append(q, m.pending[status.ChatID]...)
			delete(m.pending, old)
			moved, oldQueue, newQueue = true, m.queuedLocked(old), m.queuedLocked(status.ChatID)
		}
	}
	m.mu.Unlock()
	if moved {
		m.saveQueue(old, oldQueue)
		m.saveQueue(status.ChatID, newQueue)
	}
	m.queueMu.Unlock()
	m.hooks.Session(status)
	m.statusChanged(status, activity, time.Now())
	for _, queued := range rekeyed {
		m.statusChanged(queued, ChatQueued, time.Now())
	}
}

// Agent is one agent a turn can name and, when it cannot run here, why not.
type Agent struct {
	Kind      Kind   `json:"kind"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Agents is every adapter's availability, sorted by kind.
func (m *Manager) Agents() []Agent {
	out := make([]Agent, 0, len(m.adapters))
	for k, a := range m.adapters {
		entry := Agent{Kind: k, Available: true}
		if err := a.Available(); err != nil {
			entry.Available, entry.Reason = false, err.Error()
		}
		out = append(out, entry)
	}
	slices.SortFunc(out, func(x, y Agent) int { return strings.Compare(string(x.Kind), string(y.Kind)) })
	return out
}

// stallTimeout force-settles a turn whose agent has gone silent, or a wedged
// bridge leaves it running forever. Generous because a long tool call is normal.
const (
	stallTimeout = 10 * time.Minute
	stallSweep   = 30 * time.Second
)

var ErrNotFound = errkind.New(errkind.NotFound, "turn not found")

// ErrInvalidTurn is a request the caller got wrong, never a host failure.
var ErrInvalidTurn = errkind.New(errkind.Invalid, "invalid turn request")

// SweepStalled cancels turns that have produced nothing for stallTimeout.
// Cancelling rather than settling so execute() settles through its normal path.
func (m *Manager) SweepStalled() int {
	cutoff := time.Now().Add(-stallTimeout)

	m.mu.Lock()
	var stalled []*turn
	for _, t := range m.running {
		if t.lastEventBefore(cutoff) {
			stalled = append(stalled, t)
		}
	}
	m.mu.Unlock()

	for _, t := range stalled {
		m.log.Warn("abandoning stalled turn", "turn", t.status.TurnID, "agent", t.req.Agent,
			"silent_for", stallTimeout)
		t.mu.Lock()
		t.stalled = true
		t.mu.Unlock()
		t.cancel()
	}
	return len(stalled)
}

// Watch runs the stall sweeper until ctx ends.
func (m *Manager) Watch(ctx context.Context) {
	ticker := time.NewTicker(stallSweep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.SweepStalled()
		}
	}
}

// Send accepts a turn. It runs immediately if its chat is idle, otherwise it
// waits behind that chat's queue. Availability is checked here so a caller
// learns the CLI is missing at submit time rather than from a failed turn.
func (m *Manager) Send(req TurnRequest) (TurnStatus, error) {
	if strings.TrimSpace(req.Prompt) == "" && len(req.Attachments) == 0 {
		return TurnStatus{}, fmt.Errorf("%w: prompt is required", ErrInvalidTurn)
	}
	if req.Cwd == "" {
		return TurnStatus{}, fmt.Errorf("%w: cwd is required", ErrInvalidTurn)
	}
	if req.ChatID == "" {
		req.ChatID = uuid.NewString()
	}

	adapter, ok := m.adapters[req.Agent]
	if !ok {
		return TurnStatus{}, fmt.Errorf("%w: unknown agent %q", ErrInvalidTurn, req.Agent)
	}
	if err := adapter.Available(); err != nil {
		return TurnStatus{}, err
	}

	t := m.newTurn(req, uuid.NewString(), time.Now())

	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	m.mu.Lock()
	if time.Now().Before(m.updateUntil) {
		m.mu.Unlock()
		t.cancel()
		return TurnStatus{}, ErrUpdating
	}
	m.turns[t.status.TurnID] = t
	// Held turns wait for Send Now, so a chat holding only those runs a new
	// prompt at once.
	start := m.running[req.ChatID] == nil
	var queue []StoredTurn
	if start {
		m.running[req.ChatID] = t
	} else {
		m.pending[req.ChatID] = append(m.pending[req.ChatID], t)
		queue = m.queuedLocked(req.ChatID)
	}
	m.mu.Unlock()
	if !start {
		m.saveQueue(req.ChatID, queue)
	}

	m.statusChanged(t.snapshot(), ChatQueued, t.status.QueuedAt)
	reply := t.snapshot()
	if start {
		go m.execute(t, adapter)
		// Starting, not waiting: a caller shows "queued" only for a turn
		// that is behind another one.
		reply.State = StateRunning
	}
	return reply, nil
}

// newTurn is a turn waiting to run. Its context exists from the start so a
// Stop that lands before execute picks the turn up still cancels it.
func (m *Manager) newTurn(req TurnRequest, turnID string, queuedAt time.Time) *turn {
	ctx, cancel := context.WithCancel(context.Background())
	return &turn{
		req:        req,
		ctx:        ctx,
		cancel:     cancel,
		subs:       make(map[chan Event]struct{}),
		chatStatus: ChatQueued,
		onStatus:   m.statusChanged,
		status: TurnStatus{
			TurnID:   turnID,
			ChatID:   req.ChatID,
			Agent:    req.Agent,
			Cwd:      req.Cwd,
			Prompt:   req.Prompt,
			Model:    req.Config.Model,
			State:    StateQueued,
			QueuedAt: queuedAt,

			PermissionMode: req.Config.PermissionMode,
			Title:          req.Title,
		},
	}
}

// How long an agent may take to accept input into its running turn before
// the input queues instead.
const steerTimeout = 10 * time.Second

// Steer adds req to its chat's running turn when the agent reads input
// mid-turn. Anything else, a refusal included, sends it as Send does;
// steered says which, so a device never shows a steer that did not land.
func (m *Manager) Steer(req TurnRequest) (status TurnStatus, steered bool, err error) {
	if strings.TrimSpace(req.Prompt) == "" && len(req.Attachments) == 0 {
		return TurnStatus{}, false, fmt.Errorf("%w: prompt is required", ErrInvalidTurn)
	}
	m.mu.Lock()
	t := m.running[req.ChatID]
	m.mu.Unlock()
	steerer, ok := m.adapters[req.Agent].(Steerer)
	if t != nil && ok {
		running := t.snapshot()
		ctx, cancel := context.WithTimeout(t.ctx, steerTimeout)
		err := steerer.Steer(ctx, running.TurnID, req)
		cancel()
		if err == nil {
			return running, true, nil
		}
		m.log.Info("steer refused; queueing", "chat", req.ChatID, "turn", running.TurnID, "err", err)
	}
	status, err = m.Send(req)
	return status, false, err
}

// ErrNotQueued answers RemoveQueued or EditQueued for a turn that has already started
// (or ended): a queue row acted on late must not stop the turn it became.
var ErrNotQueued = errkind.New(errkind.Invalid, "turn is no longer queued")

// Stop cancels a running turn or drops a queued one.
func (m *Manager) Stop(turnID string) error { return m.stop(turnID, false) }

// StopChat stops what a chat is running: the Manager's turn, else a turn the
// agent started on its own. ErrNotFound means neither is running here.
func (m *Manager) StopChat(ctx context.Context, chatID string) error {
	m.mu.Lock()
	t := m.running[chatID]
	m.mu.Unlock()
	if t != nil {
		return m.Stop(t.snapshot().TurnID)
	}
	for _, a := range m.adapters {
		interrupter, ok := a.(Interrupter)
		if !ok {
			continue
		}
		stopped, err := interrupter.Interrupt(ctx, chatID)
		if err != nil || stopped {
			return err
		}
	}
	return ErrNotFound
}

// Respond answers a pending approval.
func (m *Manager) Respond(turnID, callID string, answer json.RawMessage) error {
	m.mu.Lock()
	ch, ok := m.waiting[turnID+"\x00"+callID]
	m.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	select {
	case ch <- answer:
		return nil
	default:
		// Already answered. Idempotent by design: a retried tap must not error,
		// and this is the one choke point every agent passes through.
		return nil
	}
}

// EditQueued replaces the prompt of a turn still waiting in its chat's queue,
// keeping its place. Decided under advance's queue lock: changed before it
// starts, or refused with ErrNotQueued.
func (m *Manager) EditQueued(turnID, prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("%w: prompt is required", ErrInvalidTurn)
	}
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	m.mu.Lock()
	t, ok := m.turns[turnID]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	chatID := t.snapshot().ChatID
	queued := false
	for _, p := range m.pending[chatID] {
		if p == t {
			queued = true
			break
		}
	}
	if !queued {
		m.mu.Unlock()
		return ErrNotQueued
	}
	t.req.Prompt = prompt
	queue := m.queuedLocked(chatID)
	m.mu.Unlock()
	m.saveQueue(chatID, queue)
	return nil
}

// RemoveQueued drops a turn only while it still waits in its chat's queue.
func (m *Manager) RemoveQueued(turnID string) error { return m.stop(turnID, true) }

func (m *Manager) stop(turnID string, queuedOnly bool) error {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	m.mu.Lock()
	t, ok := m.turns[turnID]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}

	// A turn still in its chat's queue has no process to kill and nothing else
	// will settle it. Once advance has popped it, execute owns it: cancel only.
	chatID := t.snapshot().ChatID
	q := m.pending[chatID]
	queued := false
	for i, p := range q {
		if p == t {
			m.pending[chatID] = append(q[:i], q[i+1:]...)
			queued = true
			break
		}
	}
	if queuedOnly && !queued {
		m.mu.Unlock()
		return ErrNotQueued
	}
	var queue []StoredTurn
	if queued {
		queue = m.queuedLocked(chatID)
	}
	m.mu.Unlock()
	if queued {
		m.saveQueue(chatID, queue)
	}

	t.cancel()
	if queued {
		t.settle(StateStopped, "stopped before it started", nil, "")
		m.forget(t)
	}
	return nil
}

// endedTurnRetention keeps a settled turn's replay around long enough for a
// late subscriber or status poll, then frees its event history.
const endedTurnRetention = 5 * time.Minute

func (m *Manager) forget(t *turn) {
	id := t.status.TurnID
	time.AfterFunc(endedTurnRetention, func() {
		m.mu.Lock()
		delete(m.turns, id)
		m.mu.Unlock()
	})
}

// asker surfaces an approval and blocks the agent until someone answers; the
// CLI waits on the reply, so returning early would let it act.
func (m *Manager) asker(t *turn) func(context.Context, Approval) (json.RawMessage, error) {
	return func(ctx context.Context, a Approval) (json.RawMessage, error) {
		key := t.status.TurnID + "\x00" + a.CallID
		ch := make(chan json.RawMessage, 1)

		m.mu.Lock()
		m.waiting[key] = ch
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			delete(m.waiting, key)
			m.mu.Unlock()
		}()

		t.publish(Event{Kind: EventApprovalRequested, Approval: &a})

		select {
		case <-ctx.Done():
			// The turn was stopped or the process died. Declining is the only
			// safe answer for an agent that is still waiting.
			return nil, ctx.Err()
		case answer := <-ch:
			t.publish(Event{Kind: EventApprovalResolved, Approval: &Approval{CallID: a.CallID, Title: string(answer)}})
			return answer, nil
		}
	}
}

func (m *Manager) Status(turnID string) (TurnStatus, error) {
	m.mu.Lock()
	t, ok := m.turns[turnID]
	m.mu.Unlock()
	if !ok {
		return TurnStatus{}, ErrNotFound
	}
	return t.snapshot(), nil
}

func (m *Manager) List() []TurnStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TurnStatus, 0, len(m.turns))
	for _, t := range m.turns {
		out = append(out, t.snapshot())
	}
	return out
}

// Subscribe replays everything the turn has emitted so far, then streams the
// rest, so a caller cannot lose the beginning. The channel closes when the
// turn ends; call unsubscribe when done.
func (m *Manager) Subscribe(turnID string) (<-chan Event, func(), error) {
	m.mu.Lock()
	t, ok := m.turns[turnID]
	m.mu.Unlock()
	if !ok {
		return nil, nil, ErrNotFound
	}
	ch, unsub := t.subscribe()
	return ch, unsub, nil
}

func (m *Manager) execute(t *turn, adapter Adapter) {
	ctx, cancel := t.ctx, t.cancel
	now := time.Now()
	t.mu.Lock()
	t.status.State = StateRunning
	t.status.StartedAt = &now
	t.lastEvent = now
	t.mu.Unlock()

	t.publish(Event{Kind: EventTurnStarted})

	m.log.Info("turn started",
		"turn", t.status.TurnID, "chat", t.req.ChatID, "agent", t.req.Agent, "cwd", t.req.Cwd)

	res, err := adapter.Send(ctx, t.req, TurnIO{
		Emit:     t.publish,
		Activity: t.activity,
		Ask:      m.asker(t),
		Session:  func(id string) { m.noteSession(t, id) },
		TurnID:   t.status.TurnID,
		Device:   t.req.Device,
	})
	wasCancelled := ctx.Err() != nil
	cancel()

	// Re-read: the session callback may have renamed the chat.
	chatID := t.snapshot().ChatID

	t.mu.Lock()
	stalled := t.stalled
	if res.StopReason != "" {
		t.status.ProviderStopReason = &res.StopReason
	}
	t.mu.Unlock()

	// Logged as well as published, so a log can tell a finished turn from one
	// blocked on an unanswered permission prompt.
	logSettled := func(state TurnState, reason string) {
		fields := []any{
			"turn", t.status.TurnID, "chat", chatID,
			"state", state, "took", time.Since(now).Round(time.Millisecond),
		}
		if reason != "" {
			fields = append(fields, "err", reason)
		}
		m.log.Info("turn settled", fields...)
	}

	switch {
	case stalled:
		reason := fmt.Sprintf("agent produced nothing for %v", stallTimeout)
		logSettled(StateFailed, reason)
		t.settle(StateFailed, reason, res.Usage, res.SessionID)
	case (err != nil && wasCancelled) || res.StopReason == "cancelled":
		logSettled(StateStopped, "stopped")
		t.settle(StateStopped, "stopped", res.Usage, res.SessionID)
	case err != nil:
		logSettled(StateFailed, err.Error())
		t.settle(StateFailed, err.Error(), res.Usage, res.SessionID)
	default:
		logSettled(StateDone, "")
		t.settle(StateDone, "", res.Usage, res.SessionID)
	}

	m.forget(t)
	m.advance(chatID)
}

// advance starts the next turn queued for a chat that is not held. Running it
// in a fresh goroutine keeps chains of queued turns from nesting execute() frames.
func (m *Manager) advance(chatID string) {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	m.mu.Lock()
	delete(m.running, chatID)
	close(m.ended)
	m.ended = make(chan struct{})
	q := m.pending[chatID]
	if len(q) == 0 {
		delete(m.pending, chatID)
		m.mu.Unlock()
		return
	}
	i := slices.IndexFunc(q, func(t *turn) bool { return !t.held })
	if i < 0 {
		m.mu.Unlock()
		return
	}
	next := q[i]
	m.pending[chatID] = slices.Delete(q, i, i+1)
	m.running[chatID] = next
	adapter := m.adapters[next.req.Agent]
	queue := m.queuedLocked(chatID)
	m.mu.Unlock()
	m.saveQueue(chatID, queue)

	go m.execute(next, adapter)
}

// Restore puts the turns a previous run left queued back in their chats'
// queues, held: each waits for Send Now. A queue under a temporary id waited
// behind a first turn the restart ended, so no chat shows it; it is dropped.
func (m *Manager) Restore(queues map[string][]StoredTurn) {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	for chatID, stored := range queues {
		if _, _, ok := SplitChatID(chatID); !ok {
			m.saveQueue(chatID, nil)
			continue
		}
		m.mu.Lock()
		for _, st := range stored {
			if _, ok := m.adapters[st.Request.Agent]; !ok {
				continue
			}
			t := m.newTurn(st.Request, st.TurnID, st.QueuedAt)
			t.held = true
			m.turns[st.TurnID] = t
			m.pending[chatID] = append(m.pending[chatID], t)
		}
		queue := m.queuedLocked(chatID)
		m.mu.Unlock()
		m.saveQueue(chatID, queue)
	}
}

// SendQueued starts a queued turn now with the request it was queued with:
// Send Now. Only while its chat runs nothing, since a chat runs one turn at a
// time; device is who pressed it, for the tools that drive that phone.
func (m *Manager) SendQueued(turnID string, device device.ID) (TurnStatus, error) {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	m.mu.Lock()
	if time.Now().Before(m.updateUntil) {
		m.mu.Unlock()
		return TurnStatus{}, ErrUpdating
	}
	t, ok := m.turns[turnID]
	if !ok {
		m.mu.Unlock()
		return TurnStatus{}, ErrNotFound
	}
	chatID := t.snapshot().ChatID
	q := m.pending[chatID]
	i := slices.Index(q, t)
	if i < 0 {
		m.mu.Unlock()
		return TurnStatus{}, ErrNotQueued
	}
	if m.running[chatID] != nil {
		m.mu.Unlock()
		return TurnStatus{}, fmt.Errorf("%w: the chat is running a turn; this one waits its turn", ErrInvalidTurn)
	}
	adapter := m.adapters[t.req.Agent]
	if err := adapter.Available(); err != nil {
		m.mu.Unlock()
		return TurnStatus{}, err
	}
	m.pending[chatID] = slices.Delete(q, i, i+1)
	t.held = false
	t.req.Device = device
	m.running[chatID] = t
	queue := m.queuedLocked(chatID)
	m.mu.Unlock()
	m.saveQueue(chatID, queue)

	go m.execute(t, adapter)
	reply := t.snapshot()
	reply.State = StateRunning
	return reply, nil
}
