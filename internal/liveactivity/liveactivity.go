// Package liveactivity keeps one Live Activity per paired phone for this
// environment. It carries counts only (working, needs you, finished), since
// Apple and the relay read every push in plaintext.
package liveactivity

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/push"
	"github.com/repogo/host/internal/relay"
	"github.com/repogo/host/internal/store"
)

const (
	// Statuses move several times a second during a turn; the lock screen
	// gets the latest at most this often. Needing the user goes at once.
	updateInterval = 5 * time.Second

	// With nothing working or waiting this long, the activity ends.
	idleTimeout = 15 * time.Minute

	// Past this without an update, the card says it may be outdated.
	staleAfter = 30 * time.Minute

	// iOS ends a Live Activity after eight hours. The host replaces it this
	// long before, while it can still end the old one.
	maxActivityAge = 8 * time.Hour
	renewBefore    = 10 * time.Minute

	// A phone that has not sent a started activity's update token gets
	// another start only after this, so a slow phone sees one alert.
	startRetry = 10 * time.Minute
)

// Counts is the activity's content-state (`LiveActivityAttributes.ContentState`).
type Counts struct {
	Working  int `json:"working"`
	NeedsYou int `json:"needsYou"`
	Finished int `json:"finished"`
}

// Chats reads chat rows; the store does.
type Chats interface {
	Info(store.ChatID) (store.Chat, error)
	Active() ([]store.Chat, error)
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
	wake      chan struct{}

	// Chats working or waiting on the user, by status.
	active map[store.ChatID]agent.ChatStatus
	// Chats that finished while the card was showing.
	finished map[store.ChatID]bool
	// The card is on the phones, or being started there.
	showing bool
	// When the last active chat settled; zero while any is active.
	idleSince time.Time
	// The last round of pushes.
	last delivery
	// Starts sent to phones that have not sent the update token yet.
	startSentAt map[device.ID]time.Time
	// When Run pushes next with no change to prompt it; zero for never.
	wakeAt time.Time
}

type delivery struct {
	counts Counts
	at     time.Time
	ok     bool
}

// New builds the driver that pushes the activity through send for the tokens
// in devices.
func New(chats Chats, devices *device.Store, send push.Sender, log *slog.Logger) *Driver {
	return &Driver{
		chats: chats, devices: devices, send: send, log: log, now: time.Now,
		pending: map[store.ChatID]bool{}, wake: make(chan struct{}, 1),
		active: map[store.ChatID]agent.ChatStatus{}, finished: map[store.ChatID]bool{}, startSentAt: map[device.ID]time.Time{},
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
	}
	for _, peer := range d.devices.Peers() {
		d.showing = d.showing || peer.Activity != nil
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

// observe moves one chat between active, finished and gone; false when the
// chat could not be read, which keeps what is known.
func (d *Driver) observe(id store.ChatID) bool {
	row, err := d.chats.Info(id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		d.log.Warn("liveactivity: could not read chat", "chat", id, "err", err)
		return false
	}
	_, wasActive := d.active[id]
	switch {
	case err != nil:
		delete(d.active, id)
		delete(d.finished, id)
	case row.Status.Active():
		d.active[id] = row.Status
		delete(d.finished, id)
	case wasActive:
		delete(d.active, id)
		if row.Status == agent.ChatCompleted || row.Status == agent.ChatFailed {
			d.finished[id] = true
		}
	}
	switch {
	case len(d.active) > 0:
		d.idleSince = time.Time{}
	case d.idleSince.IsZero():
		d.idleSince = d.now()
	}
	return true
}

// counts is the card as it stands.
func (d *Driver) counts() Counts {
	var c Counts
	for _, status := range d.active {
		switch status {
		case agent.ChatAwaitingApproval, agent.ChatAwaitingUser:
			c.NeedsYou++
		default:
			c.Working++
		}
	}
	c.Finished = len(d.finished)
	return c
}

// update pushes after a change: at once when the card is new, the user is
// needed or the work stops, else no sooner than updateInterval.
func (d *Driver) update(ctx context.Context) {
	c := d.counts()
	switch {
	case !d.showing && len(d.active) == 0:
		// Nothing to show and nothing showing; a finish alone starts nothing.
		clear(d.finished)
		return
	case !d.showing:
		d.showing = true
	case c == d.last.counts && d.last.ok:
		return
	}
	urgent := d.last.at.IsZero() || c.NeedsYou > d.last.counts.NeedsYou || c.Working+c.NeedsYou == 0
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
	c := d.counts()
	now := d.now()
	ending := len(d.active) == 0 && now.Sub(d.idleSince) >= idleTimeout
	var round peerResult
	for _, peer := range d.devices.Peers() {
		var r peerResult
		if ending {
			r = d.endPeer(ctx, peer, c)
		} else {
			r = d.pushPeer(ctx, peer, c, now)
		}
		round.retry = round.retry || r.retry
		round.nextAt = earliest(round.nextAt, r.nextAt)
	}
	d.settle(c, now, ending, round)
}

// settle records a round of pushes and sets when Run pushes next.
func (d *Driver) settle(c Counts, now time.Time, ending bool, round peerResult) {
	d.wakeAt = round.nextAt
	if round.retry {
		d.wakeAt = earliest(d.wakeAt, now.Add(updateInterval))
	}
	if ending {
		if !round.retry {
			d.showing, d.last, d.wakeAt = false, delivery{}, time.Time{}
			clear(d.finished)
			clear(d.startSentAt)
		}
		return
	}
	d.last = delivery{counts: c, at: now, ok: !round.retry}
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
func (d *Driver) pushPeer(ctx context.Context, peer device.Peer, c Counts, now time.Time) peerResult {
	if act := peer.Activity; act != nil {
		renewAt := renewalAt(*act)
		if now.Before(renewAt) {
			switch d.deliver(ctx, peer, device.PushActivity, *act, d.payload("update", c)) {
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
func (d *Driver) startPeer(ctx context.Context, peer device.Peer, c Counts, now time.Time) peerResult {
	if at, ok := d.startSentAt[peer.ID]; ok && now.Before(at.Add(startRetry)) {
		return peerResult{nextAt: at.Add(startRetry)}
	}
	if peer.PushToStart == nil || c.Working+c.NeedsYou == 0 {
		return peerResult{}
	}
	switch d.deliver(ctx, peer, device.PushToStart, *peer.PushToStart, d.payload("start", c)) {
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
func (d *Driver) endPeer(ctx context.Context, peer device.Peer, c Counts) peerResult {
	act := peer.Activity
	if act == nil {
		return peerResult{}
	}
	switch d.deliver(ctx, peer, device.PushActivity, *act, d.payload("end", c)) {
	case failed:
		return peerResult{retry: true}
	case delivered:
		d.forget(peer, device.PushActivity, act.Token)
	}
	return peerResult{}
}

// renewalAt is when a card is replaced before iOS ends it.
func renewalAt(act device.PushTarget) time.Time {
	return time.UnixMilli(act.At).Add(maxActivityAge - renewBefore)
}

// payload is the `aps` body for event. iOS drops a start without an alert,
// and issues an update token only when the start asks for one.
func (d *Driver) payload(event string, c Counts) []byte {
	now := d.now()
	aps := map[string]any{
		"timestamp": now.Unix(), "event": event, "content-state": c,
		"stale-date": now.Add(staleAfter).Unix(),
	}
	switch event {
	case "start":
		aps["attributes-type"] = "LiveActivityAttributes"
		aps["attributes"] = map[string]any{"hostId": string(d.devices.Identity().ID)}
		aps["alert"] = map[string]any{"title": "RepoGo", "body": "Working on your environment"}
		aps["input-push-token"] = 1
	case "end":
		aps["dismissal-date"] = now.Unix()
	}
	body, _ := json.Marshal(map[string]any{"aps": aps})
	return body
}

type outcome int

const (
	delivered outcome = iota
	failed            // worth trying again
	dead              // Apple will never take this token
)

// deliver sends one push and forgets a token Apple says is dead. Always high
// priority: Apple holds low-priority updates while the phone is locked.
func (d *Driver) deliver(ctx context.Context, peer device.Peer, kind string, to device.PushTarget, payload []byte) outcome {
	req := push.Request(peer, to)
	req.Payload, req.PushType, req.Priority = payload, relay.PushTypeLiveActivity, 10
	isDead, err := push.Try(ctx, d.send, req)
	switch {
	case isDead:
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
