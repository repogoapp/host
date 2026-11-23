package remotehost

import (
	"context"
	"sync"
	"time"
)

// Reuse is how long an answer stands before a caller's look runs again: a
// turn that asks twice doesn't SSH twice.
const Reuse = 2 * time.Minute

// List is the last discovery and when it ran.
type List struct {
	Hosts []Host `json:"hosts"`
	// CheckedAt is when it finished, in ms.
	CheckedAt int64 `json:"checked_at"`
}

// Service runs discovery when asked, never on a timer: nothing SSHes into the
// user's machines unless an agent or a person asked which hosts there are.
type Service struct {
	d   Deps
	now func() time.Time

	// run lets one discovery go at a time; a caller that waited on one reuses it.
	run  sync.Mutex
	mu   sync.Mutex
	last List
}

func NewService(d Deps) *Service {
	return &Service{d: d, now: time.Now, last: List{Hosts: []Host{}}}
}

// List answers the last look when it is under Reuse old, else looks again;
// fresh always looks again.
func (s *Service) List(ctx context.Context, fresh bool) List {
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
	list := List{Hosts: Discover(ctx, s.d), CheckedAt: s.now().UnixMilli()}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A look cut short reads every host as offline; the last full one is truer.
	if ctx.Err() != nil {
		return s.last
	}
	s.last = list
	return list
}

// recent is the last look when one ran under Reuse ago.
func (s *Service) recent() (List, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last.CheckedAt == 0 || s.now().Sub(time.UnixMilli(s.last.CheckedAt)) >= Reuse {
		return List{}, false
	}
	return s.last, true
}
