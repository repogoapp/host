package remotehost

import (
	"context"
	"sync"
	"time"
)

// Every is how often the background run looks again: a host that comes
// online or goes away shows within this.
const Every = 5 * time.Minute

// List is the last discovery and when it ran.
type List struct {
	Hosts []Host `json:"hosts"`
	// CheckedAt is when it finished, in ms.
	CheckedAt int64 `json:"checked_at"`
}

// Service keeps the last discovery, run in the background and on request,
// so a caller reads it without waiting on SSH.
type Service struct {
	d Deps

	// run lets one discovery go at a time; a request waits for the one running.
	run   sync.Mutex
	mu    sync.Mutex
	last  List
	first chan struct{}
	once  sync.Once
}

func NewService(d Deps) *Service {
	return &Service{d: d, first: make(chan struct{}), last: List{Hosts: []Host{}}}
}

// Run discovers now and every Every until ctx ends.
func (s *Service) Run(ctx context.Context) {
	tick := time.NewTicker(Every)
	defer tick.Stop()
	for {
		s.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Refresh discovers now and keeps the answer.
func (s *Service) Refresh(ctx context.Context) List {
	s.run.Lock()
	defer s.run.Unlock()
	list := List{Hosts: Discover(ctx, s.d), CheckedAt: time.Now().UnixMilli()}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A run cut short reads every host as offline; the last full one is truer.
	if ctx.Err() != nil {
		return s.last
	}
	s.last = list
	s.once.Do(func() { close(s.first) })
	return list
}

// Last is the kept answer, waiting for the first run if none has finished;
// an empty list when ctx ends first.
func (s *Service) Last(ctx context.Context) List {
	select {
	case <-s.first:
	case <-ctx.Done():
		return List{Hosts: []Host{}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}
