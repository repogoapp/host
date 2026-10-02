// Package emit is the one way the host tells a device something it did not ask
// for: a signal the client can refetch past, or a Stateful event whose latest
// per room is handed to whoever joins. Events are never queued.
package emit

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/wirejson"
)

// Transport carries one frame to one device. Delivery is best-effort, but
// frames to the same device must arrive in the order they were sent: a
// finished snapshot followed by its settled row is only right in that order.
type Transport interface {
	Send(to device.ID, method string, payload []byte) error
}

// Event is one thing the host can say. Method is "<family>.<name>", and the
// event itself is the payload, marshalled as JSON.
type Event interface {
	Method() string
}

// Stateful is an Event that is the current state of something in its room,
// kept per StateKey. Retain is false for the end of it (a final snapshot, a
// withdrawn prompt); Stamped returns a copy carrying its place in the order.
type Stateful interface {
	Event
	StateKey() string
	Retain() bool
	Stamped(Stamp) Stateful
}

// Delta is a Stateful whose pushes can be smaller than what its room keeps:
// members are sent Delta() of the stamped event, and a device joining later
// gets the whole event, so every later push lines up with what it holds.
type Delta interface {
	Stateful
	Delta() Event
}

// Stamp places a Stateful event in its emitter's order: the emitter's boot
// and a revision taken with the state change. Sends are not ordered with each
// other, so a device keeps the newest revision per key and drops older ones.
type Stamp struct {
	Epoch    string `json:"epoch"`
	Revision int64  `json:"revision"`
}

// Snapshot is a room's state as a device joined it, current to Stamp's
// revision: every change at or below it is reflected, every later one is sent.
type Snapshot struct {
	Stamp
	State map[string]Event
}

// Verdict is what Publish does with a Stateful event, given the room's current one.
type Verdict int

const (
	Keep Verdict = iota // set (or, if not Retain, clear) the room's state, and send
	Pass                // send, leaving the state alone
	Drop                // send nothing
)

// Room is a set of devices watching the same thing: "chat:<id>", "git:<path>".
type Room string

type Emitter struct {
	t     Transport
	log   *slog.Logger
	epoch string

	mu      sync.Mutex
	rev     int64
	members map[Room]map[device.ID]struct{}
	state   map[Room]map[string]Stateful
}

func New(t Transport, log *slog.Logger) *Emitter {
	// Per process: a restarted host's revisions start over, and a device
	// holding the old epoch must take a fresh snapshot rather than compare.
	var buf [8]byte
	rand.Read(buf[:])
	return &Emitter{
		t:       t,
		log:     log,
		epoch:   hex.EncodeToString(buf[:]),
		members: map[Room]map[device.ID]struct{}{},
		state:   map[Room]map[string]Stateful{},
	}
}

// Join adds the device to the room and returns the room's state in the same
// step, for the caller's reply: a device arriving mid-turn sees the turn, or
// that nothing is pending, without waiting for a frame.
func (e *Emitter) Join(d device.ID, r Room) Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.members[r] == nil {
		e.members[r] = map[device.ID]struct{}{}
	}
	e.members[r][d] = struct{}{}
	out := Snapshot{Stamp: Stamp{Epoch: e.epoch, Revision: e.rev}, State: map[string]Event{}}
	for key, s := range e.state[r] {
		out.State[key] = s
	}
	return out
}

func (e *Emitter) Leave(d device.ID, r Room) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.leaveLocked(d, r)
}

// LeaveAll drops the device from every room: it disconnected.
func (e *Emitter) LeaveAll(d device.ID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for r := range e.members {
		e.leaveLocked(d, r)
	}
}

func (e *Emitter) leaveLocked(d device.ID, r Room) {
	delete(e.members[r], d)
	if len(e.members[r]) == 0 {
		delete(e.members, r)
	}
}

// Members lists who is in the room, in no particular order.
func (e *Emitter) Members(r Room) []device.ID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.membersLocked(r)
}

// To sends one event to one device, outside any room: a page cut to that
// device's cursor, a reply only it is waiting for. Room state is untouched.
func (e *Emitter) To(d device.ID, ev Event) error {
	payload, err := wirejson.Marshal(ev)
	if err != nil {
		return err
	}
	return e.t.Send(d, ev.Method(), payload)
}

// Publish stamps a Stateful event and sends it as judge decides from the
// room's current state for its key (nil: Keep). Judging, stamping and storing
// are one step, so producers cannot interleave. Reports whether it was sent.
func (e *Emitter) Publish(r Room, ev Stateful, judge func(cur Event, ok bool) Verdict) bool {
	e.mu.Lock()
	verdict := Keep
	if judge != nil {
		cur, ok := e.state[r][ev.StateKey()]
		verdict = judge(cur, ok)
	}
	if verdict == Drop {
		e.mu.Unlock()
		return false
	}
	e.rev++
	stamped := ev.Stamped(Stamp{Epoch: e.epoch, Revision: e.rev})
	var sent Event = stamped
	if d, ok := stamped.(Delta); ok {
		sent = d.Delta()
	}
	payload, err := wirejson.Marshal(sent)
	if err != nil {
		e.mu.Unlock()
		e.log.Warn("emit: cannot marshal", "method", ev.Method(), "err", err)
		return false
	}
	if verdict == Keep {
		e.setStateLocked(r, stamped)
	}
	members := e.membersLocked(r)
	e.mu.Unlock()

	for _, d := range members {
		if err := e.t.Send(d, ev.Method(), payload); err != nil {
			e.log.Debug("emit: not delivered", "method", ev.Method(), "device", d, "err", err)
		}
	}
	return true
}

func (e *Emitter) setStateLocked(r Room, s Stateful) {
	key := s.StateKey()
	if !s.Retain() {
		delete(e.state[r], key)
		if len(e.state[r]) == 0 {
			delete(e.state, r)
		}
		return
	}
	if e.state[r] == nil {
		e.state[r] = map[string]Stateful{}
	}
	e.state[r][key] = s
}

func (e *Emitter) membersLocked(r Room) []device.ID {
	out := make([]device.ID, 0, len(e.members[r]))
	for d := range e.members[r] {
		out = append(out, d)
	}
	return out
}
