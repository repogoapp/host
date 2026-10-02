package agent

import (
	"sync"
	"time"
)

type poolEntry[T comparable] struct {
	session  T
	reapable func(time.Time) bool
	close    func()
	leases   int
}

// SessionPool owns processes between turns without interpreting their protocol.
type SessionPool[T comparable] struct {
	mu         sync.Mutex
	entries    map[string]poolEntry[T]
	epoch      uint64
	stop       chan struct{}
	reaperDone chan struct{}
}

func (p *SessionPool[T]) Epoch() uint64 { p.mu.Lock(); defer p.mu.Unlock(); return p.epoch }
func (p *SessionPool[T]) Get(key string) (T, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.entries[key]
	if ok {
		entry.leases++
		p.entries[key] = entry
	}
	return entry.session, ok
}

func (p *SessionPool[T]) Release(key string, s T) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.entries[key]
	if ok && entry.session == s {
		if entry.leases > 0 {
			entry.leases--
		}
		p.entries[key] = entry
	}
}

func (p *SessionPool[T]) Put(epoch uint64, key string, s T, reapable func(time.Time) bool, closeSession func()) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if epoch != p.epoch {
		return false
	}
	if p.entries == nil {
		p.entries = map[string]poolEntry[T]{}
	}
	if _, exists := p.entries[key]; exists {
		return false
	}
	p.entries[key] = poolEntry[T]{session: s, reapable: reapable, close: closeSession, leases: 1}
	if p.stop == nil {
		p.stop = make(chan struct{})
		p.reaperDone = make(chan struct{})
		go p.reap(p.stop, p.reaperDone)
	}
	return true
}

func (p *SessionPool[T]) Retire(key string, s T) {
	p.mu.Lock()
	entry, ok := p.entries[key]
	if ok && entry.session == s {
		delete(p.entries, key)
	} else {
		ok = false
	}
	p.mu.Unlock()
	if ok {
		entry.close()
	}
}

func (p *SessionPool[T]) Sweep(now time.Time) {
	p.mu.Lock()
	var dead []poolEntry[T]
	for key, entry := range p.entries {
		if entry.leases == 0 && entry.reapable(now.Add(-30*time.Minute)) {
			delete(p.entries, key)
			dead = append(dead, entry)
		}
	}
	p.mu.Unlock()
	for _, entry := range dead {
		entry.close()
	}
}

func (p *SessionPool[T]) reap(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			p.Sweep(now)
			p.mu.Lock()
			if len(p.entries) == 0 && p.stop == stop {
				p.stop = nil
				p.reaperDone = nil
				p.mu.Unlock()
				return
			}
			p.mu.Unlock()
		}
	}
}

func (p *SessionPool[T]) Close() {
	p.mu.Lock()
	live := p.entries
	p.entries = nil
	p.epoch++
	done := p.reaperDone
	p.reaperDone = nil
	if p.stop != nil {
		close(p.stop)
		p.stop = nil
	}
	p.mu.Unlock()
	if done != nil {
		<-done
	}
	for _, entry := range live {
		entry.close()
	}
}
