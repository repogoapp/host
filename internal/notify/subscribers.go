package notify

import "sync"

// subBuffer is deep enough that a burst of hooks cannot stall the drain loop.
// A subscriber that fills it is dropped: notices are live state, and a slow
// consumer wants the current state on reconnect, not a backlog.
const subBuffer = 64

type subscribers struct {
	mu  sync.Mutex
	chs map[chan Notice]struct{}
}

func newSubscribers() *subscribers {
	return &subscribers{chs: make(map[chan Notice]struct{})}
}

func (s *subscribers) add() (<-chan Notice, func()) {
	ch := make(chan Notice, subBuffer)

	s.mu.Lock()
	s.chs[ch] = struct{}{}
	s.mu.Unlock()

	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.chs[ch]; ok {
			delete(s.chs, ch)
			close(ch)
		}
	}
}

func (s *subscribers) publish(n Notice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.chs {
		select {
		case ch <- n:
		default:
			delete(s.chs, ch)
			close(ch)
		}
	}
}
