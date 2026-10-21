// Package stepactivity keeps the Live Activities plugins start on every
// paired phone: a title, a body, steps or a bar, a timer, all the plugin's. Unlike liveactivity's
// counts, its text is the plugin's and reaches Apple and the relay as is.
package stepactivity

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/push"
	"github.com/repogoapp/host/internal/relay"
)

const (
	// A plugin may update as often as it likes; each phone gets the latest
	// at most this often. A start and an end go at once.
	updateInterval = 5 * time.Second

	// APNs orders a Live Activity's pushes by a timestamp in seconds, so two
	// in one second may arrive as one.
	minGap = time.Second

	// Past this without an update, the card says it may be outdated.
	staleAfter = 30 * time.Minute

	// An ended card stays on the Lock Screen this long.
	endedFor = 15 * time.Minute

	// iOS ends a Live Activity after eight hours; the host forgets it then.
	maxAge = 8 * time.Hour

	// A phone sent a start but no update token yet gets another start only
	// after this.
	startRetry = 10 * time.Minute

	// A plugin's live activities count against perPlugin until they end or
	// go this long without an update.
	idleAfter = time.Hour
	perPlugin = 2
)

// The statuses a plugin sets; done and failed end the activity.
const (
	Running = "running"
	Done    = "done"
	Failed  = "failed"
)

// The kinds of a progress and a timer.
const (
	ProgressSteps = "steps"
	ProgressBar   = "bar"

	TimerElapsed   = "elapsed"
	TimerCountdown = "countdown"
	TimerNone      = "none"
)

// The tints a plugin may name: the cards' colors, less secondary, which
// reads as disabled on the card's black.
var tints = map[string]bool{
	"primary": true, "accent": true, "success": true, "warning": true, "danger": true,
	"blue": true, "green": true, "orange": true, "red": true, "purple": true,
	"pink": true, "teal": true, "yellow": true, "gray": true,
}

// The limits on what a plugin sends; a Live Activity's push is at most 4 KB.
const (
	maxID           = 64
	maxTitle        = 60
	maxBody         = 120
	maxLabel        = 24
	maxCompactLabel = 10
	maxSteps        = 8
	maxStep         = 24
	maxPage         = 512
	maxProject      = 1024
	maxSymbol       = 64
	maxPayload      = 4096
)

var symbolName = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9]+)*$`)

// Request is one call of a plugin's ctx.activity: the whole state, by id.
type Request struct {
	ID      string `json:"id" doc:"The activity's id within the plugin; the same id updates it"`
	Project string `json:"project,omitempty" doc:"The project whose page a tap opens"`
	Content
}

// Content is everything the card shows, sent whole on each call: a field
// left out takes its default, whatever an earlier call set.
type Content struct {
	Title        string    `json:"title"`
	Body         string    `json:"body,omitempty"`
	Page         string    `json:"page,omitempty" doc:"The project page a tap opens"`
	Symbol       string    `json:"symbol,omitempty" doc:"An SF Symbol name; the status's symbol when empty"`
	Tint         string    `json:"tint,omitempty" doc:"A named color; the status's color when empty"`
	Label        *string   `json:"label,omitempty" doc:"The card's value, \"2 of 4\"; empty hides it, null derives it"`
	CompactLabel *string   `json:"compact_label,omitempty" doc:"The island's value, \"2/4\"; null takes the label or derives it"`
	Progress     *Progress `json:"progress,omitempty" doc:"Segments or a bar; null shows neither"`
	Timer        *Timer    `json:"timer,omitempty" doc:"Elapsed from the start when null"`
	Status       string    `json:"status,omitempty" doc:"running, done or failed; done and failed end it"`
}

// Progress is a segment per step, or a bar.
type Progress struct {
	Kind    string   `json:"kind" doc:"steps or bar"`
	Steps   []string `json:"steps,omitempty" doc:"For steps: 1 to 8 names"`
	Current int      `json:"current,omitempty" doc:"For steps: the active step, 0 to len(steps); earlier ones are complete"`
	Value   float64  `json:"value,omitempty" doc:"For bar: 0 to 1"`
}

// Timer is the card's clock, which the phone ticks with no pushes.
type Timer struct {
	Kind      string `json:"kind" doc:"elapsed, countdown or none"`
	StartedAt int64  `json:"started_at,omitempty" doc:"For elapsed: Unix ms; the activity's start when 0"`
	EndsAt    int64  `json:"ends_at,omitempty" doc:"For countdown: Unix ms; reaching it ends nothing"`
}

// Attributes is what the activity starts with and keeps
// (`StepActivityAttributes` on the phone).
type Attributes struct {
	HostID  string `json:"host_id"`
	Key     string `json:"key"`
	Project string `json:"project"`
}

// State is the activity's content-state (`StepActivityAttributes.ContentState`):
// the plugin's content, its timer resolved.
type State struct {
	Content
	// EndedAt, in Unix ms, stops the timer once done or failed; 0 while running.
	EndedAt int64 `json:"ended_at"`
}

// Driver is safe for concurrent use: Set comes from plugin calls, and Run
// pushes outside the lock.
type Driver struct {
	devices *device.Store
	// send is Run's: a host without a relay never runs it, and its
	// activities go nowhere.
	send push.Sender
	log  *slog.Logger
	now  func() time.Time

	mu   sync.Mutex
	live map[string]*activity
	wake chan struct{}
}

// activity is one plugin's activity as the phones should show it.
type activity struct {
	plugin    string
	attrs     Attributes
	state     State
	startedAt time.Time
	setAt     time.Time
	// The state changed since the last round reached every phone.
	dirty    bool
	pushedAt time.Time
	// Rises with each Set, so a round's results never undo a newer one.
	revision int
	phones   map[device.ID]*phone
}

// phone is what one phone has had of an activity.
type phone struct {
	startSentAt time.Time
	// The user swiped it away: it never starts there again.
	dismissed bool
}

func New(devices *device.Store, log *slog.Logger) *Driver {
	return &Driver{
		devices: devices, log: log, now: time.Now,
		live: map[string]*activity{}, wake: make(chan struct{}, 1),
	}
}

// Set starts, updates or ends plugin's activity. Ending one that isn't
// live does nothing.
func (d *Driver) Set(plugin string, a Request) error {
	if a.Status == "" {
		a.Status = Running
	}
	if err := a.validate(); err != nil {
		return err
	}
	key := plugin + "/" + a.ID
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweep(now)
	act := d.live[key]
	fresh := act == nil
	if fresh {
		if a.Status != Running {
			return nil
		}
		if d.running(plugin, now) >= perPlugin {
			return fmt.Errorf("%w: a plugin shows at most %d activities at once; end one with status done", errkind.ErrInvalid, perPlugin)
		}
		act = &activity{
			plugin:    plugin,
			attrs:     Attributes{HostID: string(d.devices.Identity().ID), Key: key, Project: a.Project},
			startedAt: now,
			phones:    map[device.ID]*phone{},
		}
	}
	state := State{Content: a.Content}
	if state.Timer == nil {
		state.Timer = &Timer{Kind: TimerElapsed}
	}
	if state.Timer.Kind == TimerElapsed && state.Timer.StartedAt == 0 {
		state.Timer.StartedAt = act.startedAt.UnixMilli()
	}
	if a.Status != Running {
		state.EndedAt = now.UnixMilli()
	}
	r := round{key: key, attrs: act.attrs, state: state}
	if size := len(d.payload("start", r, now)); size > maxPayload {
		return invalid("the activity is %d bytes as a push, past Apple's %d; shorten its text", size, maxPayload)
	}
	if fresh {
		d.live[key] = act
	}
	act.state = state
	act.setAt, act.dirty = now, true
	act.revision++
	d.poke()
	return nil
}

// TokensChanged has Run push again: a token that arrived lets a phone get
// the updates it missed.
func (d *Driver) TokensChanged() {
	d.mu.Lock()
	for _, act := range d.live {
		act.dirty = true
	}
	d.mu.Unlock()
	d.poke()
}

// Run pushes each change through send until ctx ends.
func (d *Driver) Run(ctx context.Context, send push.Sender) {
	d.send = send
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		next := d.flush(ctx)
		timer.Stop()
		if !next.IsZero() {
			timer.Reset(max(next.Sub(d.now()), 0))
		}
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-timer.C:
		}
	}
}

func (d *Driver) poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// round is one activity's push to every phone, read under the lock.
type round struct {
	key      string
	attrs    Attributes
	state    State
	revision int
	phones   map[device.ID]phone
}

// flush pushes every activity that is due, and answers when the next one is.
func (d *Driver) flush(ctx context.Context) (next time.Time) {
	now := d.now()
	d.mu.Lock()
	d.sweep(now)
	var due []round
	for key, act := range d.live {
		if !act.dirty {
			continue
		}
		at := act.pushedAt.Add(updateInterval)
		if act.pushedAt.IsZero() || act.state.Status != Running {
			at = act.pushedAt.Add(minGap)
		}
		if now.Before(at) {
			next = earliest(next, at)
			continue
		}
		act.dirty, act.pushedAt = false, now
		r := round{key: key, attrs: act.attrs, state: act.state, revision: act.revision, phones: map[device.ID]phone{}}
		for id, p := range act.phones {
			r.phones[id] = *p
		}
		due = append(due, r)
	}
	d.mu.Unlock()

	for _, r := range due {
		if ctx.Err() != nil {
			return time.Time{}
		}
		results, retry := d.pushRound(ctx, r, now)
		d.mu.Lock()
		act := d.live[r.key]
		if act != nil {
			for id, p := range results {
				act.phones[id] = &p
			}
			switch {
			case retry:
				act.dirty = true
				next = earliest(next, now.Add(updateInterval))
			case r.state.Status != Running && act.revision == r.revision:
				delete(d.live, r.key)
			}
		}
		d.mu.Unlock()
	}
	return next
}

// pushRound sends one activity's state to every phone: an update where the
// phone has its token, else a start.
func (d *Driver) pushRound(ctx context.Context, r round, now time.Time) (map[device.ID]phone, bool) {
	ending := r.state.Status != Running
	results := map[device.ID]phone{}
	retry := false
	for _, peer := range d.devices.Peers() {
		p := r.phones[peer.ID]
		if token, ok := peer.Steps[r.key]; ok {
			event := "update"
			if ending {
				event = "end"
			}
			switch d.deliver(ctx, peer, token, d.payload(event, r, now)) {
			case delivered:
				if ending {
					d.forget(peer, r.key, token.Token)
				}
			case dead:
				d.forget(peer, r.key, token.Token)
				p.dismissed = true
			case failed:
				retry = true
			}
			results[peer.ID] = p
			continue
		}
		waiting := !p.startSentAt.IsZero() && now.Before(p.startSentAt.Add(startRetry))
		if ending || p.dismissed || waiting || peer.StepsStart == nil {
			continue
		}
		switch d.deliver(ctx, peer, *peer.StepsStart, d.payload("start", r, now)) {
		case delivered:
			p.startSentAt = now
		case dead:
			if err := d.devices.ForgetPush(peer.ID, device.PushStepsStart, peer.StepsStart.Token); err != nil {
				d.log.Warn("stepactivity: could not forget token", "device", peer.ID, "err", err)
			}
		case failed:
			retry = true
		}
		results[peer.ID] = p
	}
	return results, retry
}

// payload is the `aps` body for event. iOS drops a start without an alert,
// and issues an update token only when the start asks for one.
func (d *Driver) payload(event string, r round, now time.Time) []byte {
	aps := map[string]any{
		"timestamp": now.Unix(), "event": event, "content-state": r.state,
		"stale-date": now.Add(staleAfter).Unix(),
	}
	switch event {
	case "start":
		aps["attributes-type"] = "StepActivityAttributes"
		aps["attributes"] = r.attrs
		aps["alert"] = map[string]any{"title": r.state.Title, "body": r.state.Body}
		aps["input-push-token"] = 1
	case "end":
		aps["dismissal-date"] = now.Add(endedFor).Unix()
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

// deliver sends one push, always high priority: Apple holds low-priority
// updates while the phone is locked.
func (d *Driver) deliver(ctx context.Context, peer device.Peer, to device.PushTarget, payload []byte) outcome {
	req := push.Request(peer, to)
	req.Payload, req.PushType, req.Priority = payload, relay.PushTypeLiveActivity, 10
	isDead, err := push.Try(ctx, d.send, req)
	switch {
	case isDead:
		return dead
	case err != nil:
		d.log.Debug("stepactivity: not delivered", "device", peer.ID, "err", err)
		return failed
	}
	return delivered
}

func (d *Driver) forget(peer device.Peer, key, token string) {
	if err := d.devices.ForgetStepsPush(peer.ID, key, token); err != nil {
		d.log.Warn("stepactivity: could not forget token", "device", peer.ID, "err", err)
	}
}

// sweep forgets activities iOS has ended by now. Caller holds the lock.
func (d *Driver) sweep(now time.Time) {
	for key, act := range d.live {
		if now.Sub(act.startedAt) >= maxAge {
			delete(d.live, key)
		}
	}
}

// running is how many of plugin's activities count against perPlugin.
// Caller holds the lock.
func (d *Driver) running(plugin string, now time.Time) int {
	n := 0
	for _, act := range d.live {
		if act.plugin == plugin && act.state.Status == Running && now.Sub(act.setAt) < idleAfter {
			n++
		}
	}
	return n
}

func (a Request) validate() error {
	switch {
	case a.ID == "" || len(a.ID) > maxID:
		return invalid("id is 1 to %d bytes", maxID)
	case a.Title == "" || utf8.RuneCountInString(a.Title) > maxTitle:
		return invalid("title is 1 to %d characters", maxTitle)
	case utf8.RuneCountInString(a.Body) > maxBody:
		return invalid("body is at most %d characters", maxBody)
	case a.Label != nil && utf8.RuneCountInString(*a.Label) > maxLabel:
		return invalid("label is at most %d characters", maxLabel)
	case a.CompactLabel != nil && utf8.RuneCountInString(*a.CompactLabel) > maxCompactLabel:
		return invalid("compactLabel is at most %d characters", maxCompactLabel)
	case a.Status != Running && a.Status != Done && a.Status != Failed:
		return invalid("status is running, done or failed")
	case a.Symbol != "" && (len(a.Symbol) > maxSymbol || !symbolName.MatchString(a.Symbol)):
		return invalid("symbol is an SF Symbol name, as shippingbox.fill")
	case a.Tint != "" && !tints[a.Tint]:
		return invalid("tint is a named color, as blue or success")
	case len(a.Page) > maxPage || len(a.Project) > maxProject:
		return invalid("page or project is too long")
	}
	if err := a.Progress.validate(); err != nil {
		return err
	}
	return a.Timer.validate()
}

func (p *Progress) validate() error {
	if p == nil {
		return nil
	}
	switch p.Kind {
	case ProgressSteps:
		if len(p.Steps) == 0 || len(p.Steps) > maxSteps {
			return invalid("steps has 1 to %d names; use a bar for more", maxSteps)
		}
		if p.Current < 0 || p.Current > len(p.Steps) {
			return invalid("current is 0 to %d, the number of steps behind", len(p.Steps))
		}
		if p.Value != 0 {
			return invalid("value is for a bar")
		}
		for _, step := range p.Steps {
			if step == "" || utf8.RuneCountInString(step) > maxStep {
				return invalid("each step is 1 to %d characters", maxStep)
			}
		}
	case ProgressBar:
		if p.Value < 0 || p.Value > 1 {
			return invalid("value is 0 to 1")
		}
		if len(p.Steps) > 0 || p.Current != 0 {
			return invalid("steps and current are for steps")
		}
	default:
		return invalid("progress kind is steps or bar")
	}
	return nil
}

func (t *Timer) validate() error {
	if t == nil {
		return nil
	}
	switch t.Kind {
	case TimerElapsed:
		if t.StartedAt < 0 || t.EndsAt != 0 {
			return invalid("an elapsed timer takes startedAt, in Unix ms")
		}
	case TimerCountdown:
		if t.EndsAt <= 0 || t.StartedAt != 0 {
			return invalid("a countdown takes endsAt, in Unix ms")
		}
	case TimerNone:
		if t.StartedAt != 0 || t.EndsAt != 0 {
			return invalid("a timer of kind none takes no times")
		}
	default:
		return invalid("timer kind is elapsed, countdown or none")
	}
	return nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: activity: "+format, append([]any{errkind.ErrInvalid}, args...)...)
}

// earliest is the sooner of two times, a zero one being no time at all.
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}
