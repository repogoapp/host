// Package chatlive pushes a chat's new messages to the devices watching it:
// the host tails the provider file and sends the page the client would have
// fetched. Not a second ingest path; it writes through the same store.Sync.
package chatlive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/chatwire"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/session"
	"github.com/repogoapp/host/internal/store"
)

// coalesce is how long to wait for a burst of appends to settle; pushing per
// token would send hundreds of frames a second to a phone.
const coalesce = 250 * time.Millisecond

// flushPage is one pushed page, the same cap a client's forward read uses.
const flushPage = 500

// childPoll is how often a watched chat's subagents are checked while they
// write, and childIdle the longest wait once they have gone quiet: their
// files are not the chat's, so its watch does not see them.
const (
	childPoll = time.Second
	childIdle = 8 * time.Second
)

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

	// Attention reports what every paired device should hear about any chat:
	// approvals, questions, answers and turn ends; attention.go.
	Attention func(Attention)

	// Wire builds pushed pages as the phone receives them, as chats.messages does.
	Wire *chatwire.Builder

	// Children syncs a chat's subagents into Store and names the calls whose
	// agents changed.
	Children ChildSyncer
}

// ChildSyncer is chatsync.Syncer's per-chat child sync.
type ChildSyncer interface {
	SyncChildren(session.Meta) ([]string, error)
}

type Manager struct {
	// ctx is the host's lifetime: every tail and relay ends with it.
	ctx      context.Context
	sessions *session.Store
	db       *store.Store
	wire     *chatwire.Builder
	log      *slog.Logger
	children ChildSyncer

	turns TurnStream

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

	// Held from reading a chat's subagent calls to sending them; sendSubagents.
	subagentsMu sync.Mutex

	// The chat each device watches: one at a time, so opening another lets
	// the first go.
	subs map[device.ID]store.ChatID

	// One watch per chat with any device on it: one tail and one sync of its
	// files, whatever the number of devices.
	watches map[store.ChatID]*watch

	// The calls each device's open tool sheets show, by chat (WatchTools).
	// Apart from tails, so a sheet re-watching after a reconnect need not
	// wait for its chat to subscribe again.
	toolWatches map[device.ID]map[store.ChatID]map[string]uint64

	// Chats with a turn relaying through StreamTurn. Their session file may
	// not exist yet, and a subscriber is allowed to wait for it.
	streaming map[store.ChatID]int

	// Turns streaming from a terminal via the MessageDisplay hook; display.go.
	display map[store.ChatID]*displayTurn

	// Each chat's last closed terminal turn: a frame for it arriving after
	// Stop must not open it again.
	displayDone map[store.ChatID]string

	onAttention func(Attention)

	// Terminal turns' open permission prompt per chat, by tool call id, so the
	// tool running afterwards can say it was answered.
	hookPending map[store.ChatID]string
}

// watch follows one chat's files for every device on it, each from its own
// position. Positions are read and written under Manager.mu.
type watch struct {
	chatID store.ChatID
	cancel context.CancelFunc

	// Nudge asks the watch to flush now rather than on its next poll: a hook
	// has said the transcript changed, or a device needs its backlog.
	nudge chan struct{}

	at map[device.ID]position

	// childrenSynced is set once the watch's first subagent sync is done: a
	// device joining before then hears them from that sync, after it from attach.
	childrenSynced bool
}

// position is where one device stands in a chat: the next row it lacks, and
// the generation it holds, 0 before its first page says.
type position struct {
	next       int
	generation int64
}

// New serves d until ctx ends.
func New(ctx context.Context, d Deps) *Manager {
	return &Manager{
		ctx: ctx, sessions: d.Sessions, db: d.Store, emit: d.Emit, turns: d.Turns, log: d.Log, wire: d.Wire,
		children:    d.Children,
		onAttention: d.Attention,

		subs:        map[device.ID]store.ChatID{},
		watches:     map[store.ChatID]*watch{},
		toolWatches: map[device.ID]map[store.ChatID]map[string]uint64{},
		streamSent:  map[store.ChatID]streamSent{},
		now:         time.Now,
		streaming:   map[store.ChatID]int{},
		display:     map[store.ChatID]*displayTurn{},
		displayDone: map[store.ChatID]string{},
		hookPending: map[store.ChatID]string{},
	}
}

var ErrNoSuchChat = errors.New("chatlive: no such chat")

// Subscribe starts pushing a chat's rows from sinceIdx to one device, which
// lets go of any chat it watched before, and returns the chat's live state as
// of joining its room. What a phone without subscribe_cursor sends.
func (m *Manager) Subscribe(caller device.ID, chatID store.ChatID, sinceIdx int) (Live, error) {
	return m.open(caller, chatID, func(*Live) (position, error) {
		return position{next: sinceIdx}, nil
	})
}

// Open is Subscribe for a device opening or resuming a chat, in one call: the
// reply carries the rows after have (or the newest limit, when have is nil or
// no longer holds, with Reset), the chat's row, and the live state; pushes
// start where the page ends.
func (m *Manager) Open(caller device.ID, chatID store.ChatID, have *store.ChatCursor, limit int) (Live, error) {
	return m.open(caller, chatID, func(live *Live) (position, error) {
		page, chat, reset, err := m.db.Open(chatID, have, max(limit, 1))
		if err != nil {
			return position{}, err
		}
		out := m.wire.Page(page)
		out.TurnLive = chat.Status.Active()
		live.Page, live.Chat, live.Reset = &out, &chat, reset
		return position{next: page.NextIndex, generation: page.Generation}, nil
	})
}

// open joins the device to the chat's room, then reads (the page, if any),
// then places it on the chat's watch where the read ended. Joining first means
// a row written in between is in the page or pushed after it; a row both in
// a page and pushed is dropped by the device, which applies rows by index.
func (m *Manager) open(caller device.ID, chatID store.ChatID, read func(*Live) (position, error)) (Live, error) {
	if !m.known(chatID) {
		return Live{}, fmt.Errorf("%w: %s", ErrNoSuchChat, chatID)
	}
	m.mu.Lock()
	// Re-opening the chat it watches (a resume) keeps the watch running and
	// only moves this device's position: a new watch would replay the file.
	if m.subs[caller] != chatID {
		m.detachLocked(caller)
		m.subs[caller] = chatID
	}
	m.mu.Unlock()

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
	at, err := read(&live)
	if err != nil {
		// Given back as a failed chats.messages leaves it: not held. An open
		// that has replaced this one since keeps its own.
		m.mu.Lock()
		if m.subs[caller] == chatID {
			m.detachLocked(caller)
		}
		m.mu.Unlock()
		return Live{}, err
	}
	m.attach(caller, chatID, at)
	return live, nil
}

// known is whether chatID is a chat to subscribe to: one the cache or a
// running turn knows, else one whose transcript is on disk.
func (m *Manager) known(chatID store.ChatID) bool {
	m.mu.Lock()
	streaming := m.streaming[chatID] > 0
	m.mu.Unlock()
	if streaming {
		return true
	}
	if _, err := m.db.Info(chatID); err == nil {
		return true
	}
	_, _, err := m.sessions.Find(chatID.SessionID())
	return err == nil
}

// attach places caller on chatID's watch at at, starting the watch for its
// first device, and nudges it so a backlog reaches the device now rather
// than when the file next changes.
func (m *Manager) attach(caller device.ID, chatID store.ChatID, at position) {
	m.mu.Lock()
	if m.subs[caller] != chatID {
		// Another open from this device took over while this one read.
		m.mu.Unlock()
		return
	}
	w, running := m.watches[chatID]
	if !running {
		ctx, cancel := context.WithCancel(m.ctx)
		w = &watch{chatID: chatID, cancel: cancel, nudge: make(chan struct{}, 1), at: map[device.ID]position{}}
		m.watches[chatID] = w
		go m.run(ctx, w)
	}
	w.at[caller] = at
	synced := w.childrenSynced
	m.mu.Unlock()

	nudge(w)
	if synced {
		go m.sendSubagents(chatID, []device.ID{caller})
	}
}

// detachLocked takes caller off the chat it watches, stopping that chat's
// watch when it was the last device. Holds m.mu.
func (m *Manager) detachLocked(caller device.ID) {
	chatID, ok := m.subs[caller]
	if !ok {
		return
	}
	delete(m.subs, caller)
	m.emit.Leave(caller, chatRoom(chatID))
	w := m.watches[chatID]
	if w == nil {
		return
	}
	delete(w.at, caller)
	if len(w.at) == 0 {
		w.cancel()
		delete(m.watches, chatID)
	}
}

func nudge(w *watch) {
	select {
	case w.nudge <- struct{}{}:
	default:
	}
}

// Nudge flushes chatID's watch without waiting for its next poll. Called when
// a hook reports the provider wrote something, so the rows it saved reach the
// devices watching as soon as they are on disk.
func (m *Manager) Nudge(chatID store.ChatID) {
	m.mu.Lock()
	w := m.watches[chatID]
	m.mu.Unlock()
	if w != nil {
		nudge(w)
	}
}

// WatchTools answers the calls a device's open tool sheet shows, whole, and
// sends each again as it changes. The watch starts before the read, so a
// result landing between the two is never missed. The answer is not recorded
// as sent: an older flush may still push after it, so a flush is asked for,
// and the sheet is sent each call once more and ends on what the store holds.
func (m *Manager) WatchTools(caller device.ID, chatID store.ChatID, callIDs []string) ([]chatwire.ToolDetail, error) {
	m.mu.Lock()
	chats := m.toolWatches[caller]
	if chats == nil {
		chats = map[store.ChatID]map[string]uint64{}
		m.toolWatches[caller] = chats
	}
	if chats[chatID] == nil {
		chats[chatID] = map[string]uint64{}
	}
	for _, id := range callIDs {
		chats[chatID][id] = toolUnsent
	}
	m.mu.Unlock()

	calls, err := m.db.ToolCalls(chatID, callIDs)
	if err != nil {
		m.UnwatchTools(caller, chatID, callIDs)
		return nil, err
	}
	m.Nudge(chatID)
	return m.wire.Details(chatID.Kind(), calls), nil
}

// toolUnsent is a watched call no flush has sent its device yet.
const toolUnsent = 0

// toolSum is detail as toolWatches records it; never toolUnsent.
func toolSum(detail chatwire.ToolDetail) (uint64, error) {
	body, err := json.Marshal(detail)
	if err != nil {
		return 0, err
	}
	h := fnv.New64a()
	h.Write(body)
	return h.Sum64() | 1, nil
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
	defer m.mu.Unlock()
	m.detachLocked(caller)
	delete(m.toolWatches, caller)
}

// devicesOf is who w serves now.
func (m *Manager) devicesOf(w *watch) []device.ID {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Collect(maps.Keys(w.at))
}

func (m *Manager) run(ctx context.Context, w *watch) {
	if !m.awaitSession(ctx, w.chatID) {
		return
	}
	meta, events, err := m.sessions.Watch(ctx, w.chatID.SessionID())
	if err != nil {
		m.log.Warn("chatlive: cannot watch", "chat", w.chatID, "err", err)
		return
	}
	m.log.Info("chatlive: watching", "chat", w.chatID, "devices", len(m.devicesOf(w)))
	defer m.log.Info("chatlive: stopped watching", "chat", w.chatID)

	go m.watchChildren(ctx, w, meta)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.nudge:
		case _, ok := <-events:
			if !ok {
				return
			}
			// Drain the burst, then let it settle before doing any work.
			if !drain(ctx, events) {
				return
			}
		}
		if err := m.flush(ctx, w); err != nil {
			m.log.Warn("chatlive: flush failed", "chat", w.chatID, "err", err)
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

// watchChildren keeps a watched chat's subagents synced, apart from its rows
// so a slow pass never holds them up. The device hears where they stand now;
// every later change reaches all watchers through ChildrenChanged.
func (m *Manager) watchChildren(ctx context.Context, w *watch, meta session.Meta) {
	chatID := w.chatID
	if _, err := m.children.SyncChildren(meta.Restat()); err != nil {
		m.log.Debug("chatlive: could not sync subagents", "chat", chatID, "err", err)
	}
	m.mu.Lock()
	w.childrenSynced = true
	devices := slices.Collect(maps.Keys(w.at))
	m.mu.Unlock()
	m.sendSubagents(chatID, devices)

	wait := childPoll
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		changed, err := m.children.SyncChildren(meta.Restat())
		if err != nil {
			m.log.Debug("chatlive: could not sync subagents", "chat", chatID, "err", err)
		}
		// Quiet children are checked less often, up to childIdle.
		if len(changed) > 0 {
			wait = childPoll
		} else {
			wait = min(wait*2, childIdle)
		}
		timer.Reset(wait)
	}
}

// ChildrenChanged tells every device watching chatID where its calls' agents
// stand: a sweep or any watcher's pass may be the one that saw the change.
func (m *Manager) ChildrenChanged(chatID store.ChatID) {
	m.sendSubagents(chatID, m.emit.Members(chatRoom(chatID)))
}

// sendSubagents sends devices the chat's calls that started agents, none
// included, so a device back from a reconnect lets go of what ended. One send
// at a time, so an older read never reaches a device after a newer one.
func (m *Manager) sendSubagents(chatID store.ChatID, devices []device.ID) {
	if len(devices) == 0 {
		return
	}
	m.subagentsMu.Lock()
	defer m.subagentsMu.Unlock()
	calls, err := m.db.SubagentCalls(chatID)
	if err != nil {
		return
	}
	push := Subagents{ChatID: string(chatID), Calls: make([]SubagentCall, 0, len(calls))}
	for _, c := range calls {
		push.Calls = append(push.Calls, SubagentCall{CallID: c.CallID, Agents: c.Agents, Done: c.Done, Revision: c.Revision})
	}
	for _, d := range devices {
		if err := m.emit.To(d, push); err != nil {
			m.log.Debug("chatlive: could not push subagents", "chat", chatID, "device", d, "err", err)
		}
	}
}

// pushTools sends caller each watched call that reads differently from what
// a flush last sent it. Compared against the store, not a flush's appended
// rows: a late result rewrites a row in place, and the sweep may import it
// first. A call counts as sent only once its send succeeded, so a failed one
// goes again on the next flush. Only the chat's flush calls this.
func (m *Manager) pushTools(caller device.ID, chatID store.ChatID) error {
	m.mu.Lock()
	last := maps.Clone(m.toolWatches[caller][chatID])
	m.mu.Unlock()
	if len(last) == 0 {
		return nil
	}
	calls, err := m.db.ToolCalls(chatID, slices.Sorted(maps.Keys(last)))
	if err != nil {
		return err
	}
	for _, detail := range m.wire.Details(chatID.Kind(), calls) {
		sum, err := toolSum(detail)
		if err != nil {
			return err
		}
		if last[detail.CallID] == sum {
			continue
		}
		if err := m.emit.To(caller, Tool{ChatID: string(chatID), ToolDetail: detail}); err != nil {
			return err
		}
		m.mu.Lock()
		// Still the watch this read: a sheet opened since is owed its own send.
		if watched := m.toolWatches[caller][chatID]; watched != nil {
			if now, ok := watched[detail.CallID]; ok && now == last[detail.CallID] {
				watched[detail.CallID] = sum
			}
		}
		m.mu.Unlock()
	}
	return nil
}

// resync brings the cache up to date with meta's file, skipping one unchanged
// since its last sync (usually the replay a subscribe starts with). The file is
// re-read whole, not appended from the tail: Codex rewrites and re-segments its
// rollouts. The store writes only from the first changed row; search waits for
// the next sweep.
func (m *Manager) resync(meta session.Meta) error {
	if fp, ok, err := m.db.FingerprintOf(meta); err == nil && ok && store.Unchanged(fp, meta) {
		return nil
	}
	fresh, err := m.sessions.Events(meta)
	if err != nil {
		return err
	}
	return m.db.SyncBatch([]store.Entry{{Meta: meta, Events: fresh, DeferSearch: true}})
}

// flush re-syncs the chat's files once, then brings every device on the
// watch up to date from its own position. One device failing to receive does
// not hold up the others; it is retried on the next flush.
func (m *Manager) flush(ctx context.Context, w *watch) error {
	meta, _, err := m.sessions.Find(w.chatID.SessionID())
	if err != nil {
		return err
	}
	if err := m.resync(meta); err != nil {
		return err
	}
	m.mu.Lock()
	at := maps.Clone(w.at)
	m.mu.Unlock()
	for d, from := range at {
		to, err := m.deliver(ctx, d, w.chatID, from)
		if err == nil {
			err = m.pushTools(d, w.chatID)
		}
		if err != nil {
			m.log.Debug("chatlive: could not push rows", "chat", w.chatID, "device", d, "err", err)
		}
		m.mu.Lock()
		// A device that opened again meanwhile has a newer position: keep it.
		if cur, ok := w.at[d]; ok && cur == from {
			w.at[d] = to
		}
		m.mu.Unlock()
	}
	return nil
}

// deliver sends one device every row past from, page by page: a backlog left
// behind would wait for the provider to write again. A device on another
// generation is sent one empty page of the new one, so it reloads, even when
// the rewrite left fewer rows than it holds. Returns where the device stands.
func (m *Manager) deliver(ctx context.Context, d device.ID, chatID store.ChatID, from position) (position, error) {
	for ctx.Err() == nil {
		page, err := m.db.Messages(chatID, from.next, flushPage)
		if err != nil {
			return from, err
		}
		if from.generation != 0 && page.Generation != from.generation {
			notice := page
			notice.Events = nil
			notice.FirstIndex, notice.NextIndex = page.EventCount, page.EventCount
			notice.HasBefore = page.EventCount > 0
			if err := m.emit.To(d, Appended{Page: m.wire.Page(notice)}); err != nil {
				return from, err
			}
			return position{next: page.EventCount, generation: page.Generation}, nil
		}
		from.generation = page.Generation
		if len(page.Events) == 0 {
			return from, nil
		}
		if err := m.emit.To(d, Appended{Page: m.wire.Page(page)}); err != nil {
			return from, err
		}
		from.next = page.NextIndex
		if from.next >= page.EventCount {
			return from, nil
		}
	}
	return from, ctx.Err()
}
