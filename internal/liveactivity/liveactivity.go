// Package liveactivity keeps one Live Activity per paired phone for this
// environment: an iOS Live Activity through APNs, or an Android Live Update
// the app draws from FCM data messages: counts (working, needs you,
// finished) and the chats behind them, sealed for the phone (pushseal). A
// phone that sent no key gets no card: nothing goes out in the clear.
package liveactivity

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/push"
	"github.com/repogoapp/host/internal/pushgate"
	"github.com/repogoapp/host/internal/pushseal"
	"github.com/repogoapp/host/internal/store"
)

const (
	// Statuses move several times a second during a turn; the lock screen
	// gets the latest at most this often. Needing the user goes at once.
	updateInterval = 5 * time.Second

	// With nothing working or waiting this long, the activity ends.
	idleTimeout = 15 * time.Minute

	// Past this without an update, the card says it may be outdated.
	staleAfter = 30 * time.Minute

	// A round that keeps failing waits longer each time, up to this; an end
	// that keeps failing is given up this long after it was due, when the
	// phones have long dropped the card.
	maxRetryGap = 5 * time.Minute
	giveUpEnd   = 15 * time.Minute

	// iOS ends a Live Activity after eight hours. The host replaces it this
	// long before, while it can still end the old one.
	maxActivityAge = 8 * time.Hour
	renewBefore    = 10 * time.Minute

	// A phone that has not sent a started activity's update token gets
	// another start only after this, so a slow phone sees one alert.
	startRetry = 10 * time.Minute
)

// Counts is what the card counts (`LiveActivityAttributes.ContentState`),
// sealed with its chats in the content-state.
type Counts struct {
	Working  int `json:"working"`
	NeedsYou int `json:"needs_you"`
	Finished int `json:"finished"`
}

// Row is one chat on the card: its title and where it stands.
type Row struct {
	Title  string `json:"title"`
	Status string `json:"status"`
}

// The statuses a Row shows, and how many rows a card carries: a push is at
// most 4 KB, sealed and encoded. Older hosts sent needs_you for both
// waits and finished for every ending; apps still read those.
const (
	RowQuestion = "question"
	RowApproval = "approval"
	RowWorking  = "working"
	RowFinished = "finished"
	RowFailed   = "failed"
	RowStopped  = "stopped"

	maxRows     = 4
	maxRowTitle = 48

	// How many of the newest chats the card reads for its done rows and
	// Finished count: enough for the turns a card sees end.
	recentScan = 50
)

// chatInfo is what a row needs of a working or waiting chat.
type chatInfo struct {
	title    string
	statusAt int64
}

// Chats reads chat rows; the store does. Chats lists the newest first, for
// the done chats under the active ones.
type Chats interface {
	Info(store.ChatID) (store.Chat, error)
	Active() ([]store.Chat, error)
	Chats(store.ChatQuery) (store.ChatPage, error)
}

// Driver is owned by Run: only the pending set is touched from outside it.
type Driver struct {
	chats   Chats
	devices *device.Store
	send    push.Sender
	log     *slog.Logger
	now     func() time.Time

	// Chats a store commit moved, waiting for Run, so the store's writer
	// never reads the store or waits on the relay.
	pendingMu sync.Mutex
	pending   map[store.ChatID]bool
	// A phone registered or cleared a card token since Run last looked.
	tokensChanged bool
	wake          chan struct{}

	// Chats working or waiting on the user, by status.
	active map[store.ChatID]agent.ChatStatus
	// The title and status time of each chat in active.
	info map[store.ChatID]chatInfo
	// The card is on the phones, or being started there.
	showing bool
	// When the last active chat settled; zero while any is active.
	idleSince time.Time
	// The last round of pushes.
	last delivery
	// Rounds in a row that left a phone to retry.
	failures int
	// Starts sent to phones that have not sent the update token yet.
	startSentAt map[device.ID]time.Time
	// Android's side: the card's generation, new each time it starts showing,
	// and each round's revision, so the app ignores a message older than one
	// it drew; the Android phones that have had this generation's start.
	generation   int64
	revision     int64
	androidShown map[device.ID]bool
	// When Run pushes next with no change to prompt it; zero for never.
	wakeAt time.Time
}

type delivery struct {
	counts Counts
	rows   []Row
	at     time.Time
	ok     bool
}

// New builds the driver that pushes the activity through send for the tokens
// in devices.
func New(chats Chats, devices *device.Store, send push.Sender, log *slog.Logger) *Driver {
	return &Driver{
		chats: chats, devices: devices, send: send, log: log, now: time.Now,
		pending: map[store.ChatID]bool{}, wake: make(chan struct{}, 1),
		active: map[store.ChatID]agent.ChatStatus{}, info: map[store.ChatID]chatInfo{},
		startSentAt:  map[device.ID]time.Time{},
		androidShown: map[device.ID]bool{},
	}
}

// Changed queues the chats a store commit moved; Run reads them.
func (d *Driver) Changed(c store.Change) {
	d.pendingMu.Lock()
	for _, id := range append(c.Changed, c.Removed...) {
		d.pending[id] = true
	}
	d.pendingMu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// TokensChanged has Run push again even with the counts unchanged: a phone
// that lost its card, as on a reinstall, gets a new one.
func (d *Driver) TokensChanged() {
	d.pendingMu.Lock()
	d.tokensChanged = true
	d.pendingMu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run keeps the card in step with the chats until ctx ends.
func (d *Driver) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	d.restore(ctx)
	for {
		timer.Stop()
		if !d.wakeAt.IsZero() {
			timer.Reset(max(d.wakeAt.Sub(d.now()), 0))
		}
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
			d.observePending()
			d.update(ctx)
		case <-timer.C:
			d.observePending()
			d.push(ctx)
		}
	}
}

// restore picks up after a restart: the chats still active, and the cards
// phones still show, which end after the idle timeout if nothing is.
func (d *Driver) restore(ctx context.Context) {
	rows, err := d.chats.Active()
	if err != nil {
		d.log.Warn("liveactivity: could not read active chats", "err", err)
	}
	for _, row := range rows {
		d.active[row.ID] = row.Status
		d.info[row.ID] = infoOf(row)
	}
	for _, peer := range d.devices.Peers() {
		// An Android phone may still show a card from before the restart:
		// showing again ends it, with the counts, once nothing is active.
		d.showing = d.showing || peer.Activity != nil || push.IsFCM(peer.PushToStart)
	}
	if d.showing {
		d.generation = d.now().UnixMilli()
	}
	if len(d.active) == 0 && d.showing {
		d.idleSince = d.now()
	}
	d.update(ctx)
}

// observePending reads the queued chats. One that cannot be read stays
// queued for the next push.
func (d *Driver) observePending() {
	d.pendingMu.Lock()
	ids := d.pending
	d.pending = map[store.ChatID]bool{}
	if d.tokensChanged {
		d.tokensChanged = false
		// The last round no longer covers every phone.
		d.last.ok = false
	}
	d.pendingMu.Unlock()
	for id := range ids {
		if d.observe(id) {
			continue
		}
		d.pendingMu.Lock()
		d.pending[id] = true
		d.pendingMu.Unlock()
		d.wakeAt = earliest(d.wakeAt, d.now().Add(updateInterval))
	}
}

// observe moves one chat in or out of active; false when the chat could not
// be read, which keeps what is known.
func (d *Driver) observe(id store.ChatID) bool {
	row, err := d.chats.Info(id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		d.log.Warn("liveactivity: could not read chat", "chat", id, "err", err)
		return false
	}
	if err == nil && row.Status.Active() {
		d.active[id] = row.Status
		d.info[id] = infoOf(row)
	} else {
		delete(d.active, id)
		delete(d.info, id)
	}
	switch {
	case len(d.active) > 0:
		d.idleSince = time.Time{}
	case d.idleSince.IsZero():
		d.idleSince = d.now()
	}
	return true
}

// infoOf is a chat's row title, its own or its last message's.
func infoOf(c store.Chat) chatInfo {
	return chatInfo{title: push.Line(cmp.Or(c.Title, c.LastUserMessagePreview, "Chat"), maxRowTitle), statusAt: c.StatusAt}
}

// card is what the phones show: needing the user, then working, each newest
// first, then the newest done chats (finished, failed or stopped, as Activity
// lists them) to fill maxRows, so a chat that ends stays until newer ones take
// its row. Finished counts turns that completed or failed since the card
// started, from one read of the store.
func (d *Driver) card() card {
	var c Counts
	type ranked struct {
		Row
		at int64
	}
	var active []ranked
	for id, status := range d.active {
		r := activeRow(status)
		if r == RowWorking {
			c.Working++
		} else {
			c.NeedsYou++
		}
		active = append(active, ranked{Row{d.info[id].title, r}, d.info[id].statusAt})
	}
	slices.SortFunc(active, func(a, b ranked) int {
		return cmp.Or(cmp.Compare(rowRank(a.Status), rowRank(b.Status)), cmp.Compare(b.at, a.at), cmp.Compare(a.Title, b.Title))
	})
	rows := make([]Row, 0, maxRows)
	for _, r := range active[:min(len(active), maxRows)] {
		rows = append(rows, r.Row)
	}

	// The active ones come back in the page too.
	page, err := d.chats.Chats(store.ChatQuery{Limit: recentScan + len(d.active)})
	if err != nil {
		d.log.Warn("liveactivity: could not read recent chats", "err", err)
	}
	for _, chat := range page.Chats {
		if _, running := d.active[chat.ID]; running || chat.Status.Active() {
			continue
		}
		if (chat.Status == agent.ChatCompleted || chat.Status == agent.ChatFailed) && chat.StatusAt >= d.generation {
			c.Finished++
		}
		if len(rows) < maxRows {
			rows = append(rows, Row{infoOf(chat).title, doneRow(chat.Status)})
		}
	}
	return card{c, rows}
}

// activeRow is a running chat's row status: what it waits on, or working.
func activeRow(status agent.ChatStatus) string {
	switch status {
	case agent.ChatAwaitingApproval:
		return RowApproval
	case agent.ChatAwaitingUser:
		return RowQuestion
	}
	return RowWorking
}

// doneRow is how an ended chat's turn ended: failed, stopped or finished.
func doneRow(status agent.ChatStatus) string {
	switch status {
	case agent.ChatFailed:
		return RowFailed
	case agent.ChatCancelled, agent.ChatInterrupted:
		return RowStopped
	}
	return RowFinished
}

// rowRank puts the chats needing the user above the working ones.
func rowRank(status string) int {
	if status == RowWorking {
		return 1
	}
	return 0
}

// update pushes after a change: at once when the card is new, the user is
// needed or the work stops, else no sooner than updateInterval.
func (d *Driver) update(ctx context.Context) {
	switch {
	case !d.showing && len(d.active) == 0:
		// Nothing to show and nothing showing; a finish alone starts nothing.
		return
	case !d.showing:
		d.showing = true
		d.generation = d.now().UnixMilli()
		clear(d.androidShown)
	}
	c := d.card()
	if c.Counts == d.last.counts && slices.Equal(c.Chats, d.last.rows) && d.last.ok {
		return
	}
	urgent := d.urgent(c.Counts)
	if next := d.last.at.Add(updateInterval); !urgent && d.now().Before(next) {
		d.wakeAt = earliest(d.wakeAt, next)
		return
	}
	d.push(ctx)
}

// push sends the counts to every phone, or ends the card once it has been
// idle for idleTimeout.
func (d *Driver) push(ctx context.Context) {
	if !d.showing || ctx.Err() != nil {
		return
	}
	c := d.card()
	now := d.now()
	ending := len(d.active) == 0 && now.Sub(d.idleSince) >= idleTimeout
	// Android gets running counts at normal priority; these wake the phone.
	urgent := ending || d.urgent(c.Counts)
	d.revision++
	var round peerResult
	for _, peer := range d.devices.Peers() {
		// Nothing goes out in the clear: a phone with no key gets no card.
		if push.SealKey(peer) == nil {
			continue
		}
		var r peerResult
		switch {
		case push.IsFCM(peer.PushToStart):
			r = d.pushAndroid(ctx, peer, c, ending, urgent)
		case ending:
			r = d.endPeer(ctx, peer, c)
		default:
			r = d.pushPeer(ctx, peer, c, now)
		}
		round.retry = round.retry || r.retry
		round.nextAt = earliest(round.nextAt, r.nextAt)
	}
	d.settle(c, now, ending, round)
}

// urgent is a change worth pushing at once and waking the phone for: the
// first, the user newly needed, or the work stopping.
func (d *Driver) urgent(c Counts) bool {
	return d.last.at.IsZero() || c.NeedsYou > d.last.counts.NeedsYou || c.Working+c.NeedsYou == 0
}

// settle records a round of pushes and sets when Run pushes next.
func (d *Driver) settle(c card, now time.Time, ending bool, round peerResult) {
	d.wakeAt = round.nextAt
	if round.retry {
		d.failures++
		d.wakeAt = earliest(d.wakeAt, now.Add(push.Backoff(d.failures, updateInterval, maxRetryGap)))
	} else {
		d.failures = 0
	}
	if ending {
		if !round.retry || now.Sub(d.idleSince) >= idleTimeout+giveUpEnd {
			d.showing, d.last, d.wakeAt = false, delivery{}, time.Time{}
			clear(d.startSentAt)
			clear(d.androidShown)
		}
		return
	}
	d.last = delivery{counts: c.Counts, rows: c.Chats, at: now, ok: !round.retry}
	if len(d.active) == 0 {
		d.wakeAt = earliest(d.wakeAt, d.idleSince.Add(idleTimeout))
	}
}

// peerResult is what one phone needs next: a retry, or a push at nextAt.
type peerResult struct {
	retry  bool
	nextAt time.Time
}

// pushPeer updates the phone's card, or starts one where it has none, its
// card was dismissed, or the card is due for renewal.
func (d *Driver) pushPeer(ctx context.Context, peer device.Peer, c card, now time.Time) peerResult {
	if act := peer.Activity; act != nil {
		renewAt := renewalAt(*act)
		if now.Before(renewAt) {
			switch d.deliver(ctx, peer, device.PushActivity, *act, "update", c) {
			case delivered:
				delete(d.startSentAt, peer.ID)
				return peerResult{nextAt: renewAt}
			case failed:
				return peerResult{retry: true}
			}
			// Dead: the user dismissed it or iOS ended it.
		} else if r := d.endPeer(ctx, peer, c); r.retry {
			return r
		}
	}
	return d.startPeer(ctx, peer, c, now)
}

// startPeer starts a card on the phone while there is work to show, at most
// once per startRetry until its update token arrives. A start still waiting
// on its token wakes the loop then, so the card gets the counts since.
func (d *Driver) startPeer(ctx context.Context, peer device.Peer, c card, now time.Time) peerResult {
	if at, ok := d.startSentAt[peer.ID]; ok && now.Before(at.Add(startRetry)) {
		return peerResult{nextAt: at.Add(startRetry)}
	}
	if peer.PushToStart == nil || c.Working+c.NeedsYou == 0 {
		return peerResult{}
	}
	switch d.deliver(ctx, peer, device.PushToStart, *peer.PushToStart, "start", c) {
	case delivered:
		d.startSentAt[peer.ID] = now
		// Then: the card's token, if it came, gets this round's renewal time.
		return peerResult{nextAt: now.Add(startRetry)}
	case failed:
		return peerResult{retry: true}
	}
	return peerResult{}
}

// endPeer ends the phone's card and forgets its token.
func (d *Driver) endPeer(ctx context.Context, peer device.Peer, c card) peerResult {
	act := peer.Activity
	if act == nil {
		return peerResult{}
	}
	switch d.deliver(ctx, peer, device.PushActivity, *act, "end", c) {
	case failed:
		return peerResult{retry: true}
	case delivered:
		d.forget(peer, device.PushActivity, act.Token)
	}
	return peerResult{}
}

// pushAndroid sends the counts to an Android phone as a data message, which
// its app draws as a Live Update. One token serves every round: no start
// token to wait on and no renewal, as Android doesn't end the card itself.
func (d *Driver) pushAndroid(ctx context.Context, peer device.Peer, c card, ending, urgent bool) peerResult {
	event := "update"
	switch {
	case ending:
		event = "end"
	case !d.androidShown[peer.ID]:
		event, urgent = "start", true
	}
	host := string(d.devices.Identity().ID)
	req := push.Request(peer, *peer.PushToStart)
	sealed, err := push.SealData(push.SealKey(peer), host, push.Data(map[string]any{
		"kind": "agents", "host_id": host, "event": event,
		"generation": strconv.FormatInt(d.generation, 10), "revision": strconv.FormatInt(d.revision, 10),
		"timestamp": strconv.FormatInt(d.now().UnixMilli(), 10), "state": c.Counts, "chats": c.Chats,
	}))
	if err != nil {
		d.log.Error("liveactivity: could not seal", "device", peer.ID, "err", err)
		return peerResult{retry: true}
	}
	req.Payload, req.Priority = sealed, 5
	if urgent {
		req.Priority = 10
	}
	isDead, err := push.Try(ctx, d.send, req)
	switch {
	case isDead:
		d.log.Warn("liveactivity: token is dead", "device", peer.ID, "service", "fcm", "err", err)
		d.forget(peer, device.PushToStart, peer.PushToStart.Token)
		return peerResult{}
	case err != nil:
		d.log.Debug("liveactivity: not delivered", "device", peer.ID, "service", "fcm", "err", err)
		return peerResult{retry: true}
	}
	d.androidShown[peer.ID] = true
	return peerResult{}
}

// renewalAt is when a card is replaced before iOS ends it.
func renewalAt(act device.PushTarget) time.Time {
	return time.UnixMilli(act.At).Add(maxActivityAge - renewBefore)
}

// card is one round's content: the counts and the chats behind them, read
// once and sent to every phone.
type card struct {
	Counts
	Chats []Row
}

// sealedCounts is what an agents.state box holds: the counts and when they
// were sent, so the widget can tell a replayed state from a newer one.
type sealedCounts struct {
	Counts
	Chats []Row `json:"chats,omitempty"`
	At    int64 `json:"at"`
}

// payload is the `aps` body for event, its content-state sealed under key.
// iOS drops a start without an alert, and issues an update token only when
// the start asks for one.
func (d *Driver) payload(event string, c card, key []byte) ([]byte, error) {
	now := d.now()
	host := string(d.devices.Identity().ID)
	env, err := pushseal.Seal(key, host, pushseal.AgentsState, "", sealedCounts{c.Counts, c.Chats, now.UnixMilli()})
	if err != nil {
		return nil, err
	}
	aps := map[string]any{
		"timestamp": now.Unix(), "event": event, "content-state": push.Sealed{Sealed: env},
		"stale-date": now.Add(staleAfter).Unix(),
	}
	switch event {
	case "start":
		aps["attributes-type"] = "LiveActivityAttributes"
		aps["attributes"] = map[string]any{"host_id": host}
		aps["alert"] = map[string]any{"title": "RepoGo", "body": "Working on your environment"}
		aps["input-push-token"] = 1
	case "end":
		aps["dismissal-date"] = now.Unix()
	}
	return json.Marshal(map[string]any{"aps": aps})
}

type outcome int

const (
	delivered outcome = iota
	failed            // worth trying again
	dead              // the push service will never take this token
)

// deliver sends one event's push and forgets a token Apple says is dead.
// Always high priority: Apple holds low-priority updates while the phone is
// locked. A push that can't be sealed is a retry, never a dead token.
func (d *Driver) deliver(ctx context.Context, peer device.Peer, kind string, to device.PushTarget, event string, c card) outcome {
	payload, err := d.payload(event, c, push.SealKey(peer))
	if err != nil {
		d.log.Error("liveactivity: could not seal", "device", peer.ID, "err", err)
		return failed
	}
	req := push.Request(peer, to)
	req.Payload, req.PushType, req.Priority = payload, pushgate.PushTypeLiveActivity, 10
	isDead, err := push.Try(ctx, d.send, req)
	switch {
	case isDead:
		d.log.Warn("liveactivity: token is dead", "device", peer.ID, "kind", kind, "environment", to.Environment, "err", err)
		d.forget(peer, kind, to.Token)
		return dead
	case err != nil:
		d.log.Debug("liveactivity: not delivered", "device", peer.ID, "kind", kind, "err", err)
		return failed
	}
	return delivered
}

func (d *Driver) forget(peer device.Peer, kind, token string) {
	if err := d.devices.ForgetPush(peer.ID, kind, token); err != nil {
		d.log.Warn("liveactivity: could not forget token", "device", peer.ID, "kind", kind, "err", err)
	}
}

// earliest is the sooner of two times, a zero one being no time at all.
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}
