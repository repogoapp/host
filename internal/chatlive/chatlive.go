// Package chatlive pushes a chat's new messages to the devices watching it:
// the host tails the provider file and sends the page the client would have
// fetched. Not a second ingest path; it writes through the same store.Sync.
package chatlive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/chatwire"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
)

// coalesce is how long to wait for a burst of appends to settle; pushing per
// token would send hundreds of frames a second to a phone.
const coalesce = 250 * time.Millisecond

// flushPage is one pushed page, the same cap a client's forward read uses.
const flushPage = 500

// sessionWait bounds how long a subscriber to a chat this host just started
// waits for the provider to write its first line.
const sessionWait = 2 * time.Minute

// Deps is what a Manager serves from. Every field is required.
type Deps struct {
	Sessions *session.Store
	Store    *store.Store
	Emit     *emit.Emitter
	Turns    TurnStream
	Log      *slog.Logger

	// HostID stamps pushed pages, so a pushed page and a fetched one describe
	// themselves identically.
	HostID device.ID

	// Attention reports what every paired device should hear about any chat:
	// approvals, questions, answers and turn ends; attention.go.
	Attention func(Attention)

	// Approval reports each prompt this host runs and its withdrawal (nil),
	// for the Live Activity's approval buttons.
	Approval func(chatID, turnID string, approval *agent.Approval)

	// Wire builds pushed pages as the phone receives them, as chats.messages does.
	Wire *chatwire.Builder
}

type Manager struct {
	// ctx is the host's lifetime: every tail and relay ends with it.
	ctx      context.Context
	sessions *session.Store
	db       *store.Store
	wire     *chatwire.Builder
	log      *slog.Logger

	hostID device.ID
	turns  TurnStream

	// Every push leaves through here. A chat's watchers are its room, and the
	// in-flight snapshot and blocked prompt are room state a late subscriber
	// gets at once rather than after the next token.
	emit *emit.Emitter

	// Held from reading a terminal turn's text to publishing it, so a frame
	// read before Stop cannot be published after Stop's closing snapshot.
	displayMu sync.Mutex

	// Held from choosing a streaming push's delta to publishing it, so pushes
	// reach a chat's watchers in the order their offsets assume.
	streamMu   sync.Mutex
	streamSent map[store.ChatID]streamSent
	now        func() time.Time

	mu sync.Mutex

	// The file tail each device has open. One per device: a tailer is not
	// left running for every chat ever opened.
	tails map[device.ID]*tail

	// The calls each device's open tool sheets show, by chat (WatchTools).
	// Apart from tails, so a sheet re-watching after a reconnect need not
	// wait for its chat to subscribe again.
	toolWatches map[device.ID]map[store.ChatID]map[string]bool

	// Chats with a turn relaying through StreamTurn. Their session file may
	// not exist yet, and a subscriber is allowed to wait for it.
	streaming map[store.ChatID]int

	// Turns streaming from a terminal via the MessageDisplay hook; display.go.
	display map[store.ChatID]*displayTurn

	// Each chat's last closed terminal turn: a frame for it arriving after
	// Stop must not open it again.
	displayDone map[store.ChatID]string

	onApproval  func(chatID, turnID string, approval *agent.Approval)
	onAttention func(Attention)

	// Terminal turns' open permission prompt per chat, by tool call id, so the
	// tool running afterwards can say it was answered.
	hookPending map[store.ChatID]string
}

type tail struct {
	chatID store.ChatID
	cancel context.CancelFunc

	// Nudge asks the tailer to flush now rather than on its next poll: a hook
	// has just said the transcript changed.
	nudge chan struct{}
}

// New serves d until ctx ends.
func New(ctx context.Context, d Deps) *Manager {
	return &Manager{
		ctx: ctx, sessions: d.Sessions, db: d.Store, emit: d.Emit, turns: d.Turns, log: d.Log, wire: d.Wire,
		hostID: d.HostID, onApproval: d.Approval, onAttention: d.Attention,

		tails:       map[device.ID]*tail{},
		toolWatches: map[device.ID]map[store.ChatID]map[string]bool{},
		streamSent:  map[store.ChatID]streamSent{},
		now:         time.Now,
		streaming:   map[store.ChatID]int{},
		display:     map[store.ChatID]*displayTurn{},
		displayDone: map[store.ChatID]string{},
		hookPending: map[store.ChatID]string{},
	}
}

var ErrNoSuchChat = errors.New("chatlive: no such chat")

// Subscribe starts pushing a chat's tail to one device, replacing any previous
// subscription so a tailer is not left running for every chat ever opened,
// and returns the chat's live state as of joining its room.
func (m *Manager) Subscribe(caller device.ID, chatID store.ChatID, sinceIdx int) (Live, error) {
	m.mu.Lock()
	expected := m.streaming[chatID] > 0
	m.mu.Unlock()
	if !expected {
		_, err := m.db.Info(chatID)
		expected = err == nil
	}
	if _, _, err := m.sessions.Find(chatID.SessionID()); err != nil && !expected {
		return Live{}, fmt.Errorf("%w: %s", ErrNoSuchChat, chatID)
	}

	ctx, cancel := context.WithCancel(m.ctx)

	m.mu.Lock()
	if old, ok := m.tails[caller]; ok {
		old.cancel()
		m.emit.Leave(caller, chatRoom(old.chatID))
	}
	m.tails[caller] = &tail{chatID: chatID, cancel: cancel, nudge: make(chan struct{}, 1)}
	m.mu.Unlock()

	go m.run(ctx, caller, chatID, sinceIdx)

	// Pushes after joining carry later revisions than the snapshot, so the
	// client can tell which of the two is newer whatever order they arrive in.
	snap := m.emit.Join(caller, chatRoom(chatID))
	live := Live{Stamp: snap.Stamp}
	if s, ok := snap.State[Streaming{}.StateKey()].(Streaming); ok {
		live.Streaming = &s
	}
	if a, ok := snap.State[Approval{}.StateKey()].(Approval); ok {
		live.Approval = &a
	}
	return live, nil
}

// Nudge flushes every subscription on chatID without waiting for the tail's
// next poll. Called when a hook reports the provider wrote something, so the
// rows it saved reach the devices watching as soon as they are on disk.
func (m *Manager) Nudge(chatID store.ChatID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tails {
		if t.chatID != chatID {
			continue
		}
		select {
		case t.nudge <- struct{}{}:
		default:
		}
	}
}

// WatchTools sends a device the calls its open tool sheet shows, whole, as
// their rows land while it holds the chat's subscription.
func (m *Manager) WatchTools(caller device.ID, chatID store.ChatID, callIDs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	chats := m.toolWatches[caller]
	if chats == nil {
		chats = map[store.ChatID]map[string]bool{}
		m.toolWatches[caller] = chats
	}
	if chats[chatID] == nil {
		chats[chatID] = map[string]bool{}
	}
	for _, id := range callIDs {
		chats[chatID][id] = true
	}
}

// UnwatchTools stops sending the calls a closed sheet showed. Unsubscribe
// and a disconnect drop them all.
func (m *Manager) UnwatchTools(caller device.ID, chatID store.ChatID, callIDs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range callIDs {
		delete(m.toolWatches[caller][chatID], id)
	}
}

func (m *Manager) Unsubscribe(caller device.ID) {
	m.mu.Lock()
	t, ok := m.tails[caller]
	delete(m.tails, caller)
	delete(m.toolWatches, caller)
	m.mu.Unlock()
	if ok {
		t.cancel()
		m.emit.Leave(caller, chatRoom(t.chatID))
	}
}

func (m *Manager) run(ctx context.Context, caller device.ID, chatID store.ChatID, sinceIdx int) {
	if !m.awaitSession(ctx, chatID) {
		return
	}
	_, events, err := m.sessions.Watch(ctx, chatID.SessionID())
	if err != nil {
		m.log.Warn("chatlive: cannot watch", "chat", chatID, "err", err)
		return
	}
	m.log.Info("chatlive: watching", "chat", chatID, "device", caller, "from", sinceIdx)
	defer m.log.Info("chatlive: stopped watching", "chat", chatID, "device", caller)

	m.mu.Lock()
	var nudge <-chan struct{}
	if t, ok := m.tails[caller]; ok && t.chatID == chatID {
		nudge = t.nudge
	}
	m.mu.Unlock()

	cursor := sinceIdx
	for {
		select {
		case <-ctx.Done():
			return
		case <-nudge:
			next, err := m.flush(ctx, caller, chatID, cursor)
			if err != nil {
				m.log.Warn("chatlive: flush failed", "chat", chatID, "err", err)
				continue
			}
			cursor = next
		case _, ok := <-events:
			if !ok {
				return
			}
			// Drain the burst, then let it settle before doing any work.
			if !drain(ctx, events) {
				return
			}
			next, err := m.flush(ctx, caller, chatID, cursor)
			if err != nil {
				m.log.Warn("chatlive: flush failed", "chat", chatID, "err", err)
				continue
			}
			cursor = next
		}
	}
}

// awaitSession covers chats announced by session/new or an external hook before
// the provider writes its transcript.
func (m *Manager) awaitSession(ctx context.Context, chatID store.ChatID) bool {
	deadline := time.NewTimer(sessionWait)
	defer deadline.Stop()
	for {
		if _, _, err := m.sessions.Find(chatID.SessionID()); err == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			m.log.Warn("chatlive: session never appeared", "chat", chatID)
			return false
		case <-time.After(2 * coalesce):
		}
	}
}

// drain swallows everything already queued plus whatever arrives during the
// coalesce window. Reports false if the watch ended.
func drain(ctx context.Context, events <-chan agent.Event) bool {
	timer := time.NewTimer(coalesce)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case _, ok := <-events:
			if !ok {
				return false
			}
			// Keep swallowing; the timer is not reset, so a continuously
			// streaming turn still gets a push every `coalesce`.
		case <-timer.C:
			return true
		}
	}
}

// pushTools sends each watched call among rows, merged with its earlier rows.
func (m *Manager) pushTools(caller device.ID, chatID store.ChatID, rows []store.Message) error {
	m.mu.Lock()
	watched := maps.Clone(m.toolWatches[caller][chatID])
	m.mu.Unlock()
	if len(watched) == 0 {
		return nil
	}
	var ids []string
	for _, row := range rows {
		var call struct {
			CallID string `json:"call_id"`
		}
		if row.Tool != "" && json.Unmarshal([]byte(row.Tool), &call) == nil && watched[call.CallID] &&
			!slices.Contains(ids, call.CallID) {
			ids = append(ids, call.CallID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	calls, err := m.db.ToolCalls(chatID, ids)
	if err != nil {
		return err
	}
	for _, detail := range m.wire.Details(chatID.Kind(), calls) {
		if err := m.emit.To(caller, Tool{ChatID: string(chatID), ToolDetail: detail}); err != nil {
			return err
		}
	}
	return nil
}

// flush re-syncs the session and sends whatever is new, every page of it: a
// backlog left behind would wait for the provider to write again. Returns the
// cursor to resume from.
func (m *Manager) flush(ctx context.Context, caller device.ID, chatID store.ChatID, cursor int) (int, error) {
	meta, _, err := m.sessions.Find(chatID.SessionID())
	if err != nil {
		return cursor, err
	}
	// Re-read in full rather than appending the tailed events: Codex rewrites
	// and re-segments its rollouts, so a transcript is the concatenation of its
	// files, not a stream we can safely accumulate.
	fresh, err := m.sessions.Events(meta)
	if err != nil {
		return cursor, err
	}
	if err := m.db.SyncBatch([]store.Entry{{Meta: meta, Events: fresh}}); err != nil {
		return cursor, err
	}

	for ctx.Err() == nil {
		page, err := m.db.Messages(chatID, cursor, flushPage)
		if err != nil {
			return cursor, err
		}
		if len(page.Events) == 0 {
			return cursor, nil
		}
		page.HostID = string(m.hostID)
		if err := m.emit.To(caller, Appended{Page: m.wire.Page(page)}); err != nil {
			return cursor, err
		}
		if err := m.pushTools(caller, chatID, page.Events); err != nil {
			return cursor, err
		}
		cursor = page.NextIdx
		if cursor >= page.EventCount {
			return cursor, nil
		}
	}
	return cursor, nil
}
