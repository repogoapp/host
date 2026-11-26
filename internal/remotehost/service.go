package remotehost

import (
	"context"
	"sync"
	"time"
)

// Reuse is how long an answer stands before a caller's look runs again: a
// turn that asks twice doesn't SSH twice.
const Reuse = 2 * time.Minute

// Look is one discovery and when it ran. (Not List: the phones' generated
// contracts name a type after it, and Kotlin has a List.)
type Look struct {
	// Listing is whether the user asked for their SSH connections to be
	// listed (Service.SetListing). Until they do, ~/.ssh/config isn't read
	// and Hosts is empty.
	Listing bool   `json:"listing"`
	Hosts   []Host `json:"hosts"`
	// CheckedAt is when it finished, in ms.
	CheckedAt int64 `json:"checked_at"`
}

// Service runs discovery when asked, never on a timer, reads ~/.ssh/config
// only once the user asked for the list, and dials only the hosts they
// switched on: a look SSHes into nothing they didn't pick.
type Service struct {
	d   Deps
	now func() time.Time

	// run lets one discovery go at a time; a caller that waited on one reuses it.
	run  sync.Mutex
	mu   sync.Mutex
	last Look
	// slots holds each host's calls in flight, by alias (Call).
	slots map[string]chan struct{}
	// off is closed, per alias, when the user switches it off: every call to
	// it ends (whileOn).
	off map[string]chan struct{}
	// choices is what the user asked for: the list, and the hosts switched on.
	choices *choices
}

func NewService(d Deps) *Service {
	return &Service{d: d, now: time.Now, last: Look{Hosts: []Host{}}, slots: map[string]chan struct{}{},
		off: map[string]chan struct{}{}, choices: &choices{path: d.AuthorizedPath}}
}

// List answers the last look when it is under Reuse old, else looks again;
// fresh always looks again. Each host says whether the user switched it on.
func (s *Service) List(ctx context.Context, fresh bool) Look {
	last := s.look(ctx, fresh)
	out := Look{Listing: last.Listing, Hosts: make([]Host, len(last.Hosts)), CheckedAt: last.CheckedAt}
	for i, h := range last.Hosts {
		out.Hosts[i] = s.marked(h)
	}
	return out
}

// look is List before each host is marked: what is kept between looks.
func (s *Service) look(ctx context.Context, fresh bool) Look {
	if !fresh {
		if last, ok := s.recent(); ok {
			return last
		}
	}
	s.run.Lock()
	defer s.run.Unlock()
	// A look that finished while this one waited is as fresh as one it would run.
	if last, ok := s.recent(); ok && !fresh {
		return last
	}
	began := time.Now()
	list := Look{Hosts: []Host{}, CheckedAt: s.now().UnixMilli()}
	// The file is opened only for a user who asked for the list.
	if list.Listing = s.choices.listing(); list.Listing {
		list.Hosts = Discover(ctx, s.d, s.choices.has)
	}
	s.d.debug("hosts: looked", "listing", list.Listing, "hosts", len(list.Hosts), "fresh", fresh,
		"cut_short", ctx.Err() != nil, "ms", time.Since(began).Milliseconds())
	// A look cut short reads every host as offline; the last full one is truer.
	if ctx.Err() != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.last
	}
	for i, h := range list.Hosts {
		list.Hosts[i] = s.pinned(ctx, h)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = list
	return list
}

// pinned is h once the id it answered with is held against the one saved for
// its alias. A machine answering with another id isn't the one the user
// switched on: it is off again, and says so until the next look.
func (s *Service) pinned(ctx context.Context, h Host) Host {
	if h.State != Ready {
		return h
	}
	same, err := s.choices.pin(h.ID, h.HostID)
	if err != nil {
		// Not kept, so not used: nothing would hold the next answer to this one.
		return Host{ID: h.ID, Address: h.Address, State: Offline, Reason: "couldn't save which environment this is: " + err.Error()}
	}
	if same {
		return h
	}
	s.d.debug("hosts: another id answered, switched off", "alias", h.ID, "host_id", h.HostID)
	s.hangUp(ctx, h.ID)
	s.d.Changed()
	return Host{ID: h.ID, Address: h.Address, State: Off,
		Reason: "another environment answers there than the one you switched on; switch it on again if that's right"}
}

// whileOn is ctx for a call to alias: ended when the user switches alias
// off, as well as when ctx is.
func (s *Service) whileOn(ctx context.Context, alias string) (context.Context, context.CancelFunc) {
	s.mu.Lock()
	off := s.off[alias]
	if off == nil {
		off = make(chan struct{})
		s.off[alias] = off
	}
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-off:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// hangUp ends every call to alias and the connection kept open to it, so one
// that is off holds no login: also for a caller that has gone, within a few
// seconds.
func (s *Service) hangUp(ctx context.Context, alias string) {
	s.mu.Lock()
	if off := s.off[alias]; off != nil {
		close(off)
		delete(s.off, alias)
	}
	s.mu.Unlock()
	if s.d.Close == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	s.d.Close(ctx, alias)
	s.d.debug("hosts: hung up", "alias", alias)
}

// recent is the last look when one ran under Reuse ago.
func (s *Service) recent() (Look, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last.CheckedAt == 0 || s.now().Sub(time.UnixMilli(s.last.CheckedAt)) >= Reuse {
		return Look{}, false
	}
	return s.last, true
}
