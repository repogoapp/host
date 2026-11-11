package chats

import (
	"sync"
	"time"

	"github.com/repogoapp/host/internal/device"
)

// sentTurns remembers each send by the id its device gave it, so a send the
// phone repeats after its connection dropped (the reply lost on the way)
// gets the first one's answer instead of running the message twice. A repeat
// that arrives while the first is still starting waits for it.
type sentTurns struct {
	mu   sync.Mutex
	sent map[sentKey]*sentTurn
}

type sentKey struct {
	device device.ID
	id     string
}

type sentTurn struct {
	done   chan struct{}
	result any
	err    error
	at     time.Time
}

// sentTTL is how long a send id is remembered: well past the phone's
// reconnect and resend, short enough that the map stays small.
const sentTTL = 10 * time.Minute

func newSentTurns() *sentTurns { return &sentTurns{sent: map[sentKey]*sentTurn{}} }

// once runs send for the first call with this device and id, and hands every
// repeat its result. An empty id always runs: older apps send none.
func once[T any](s *sentTurns, dev device.ID, id string, send func() (T, error)) (T, error) {
	if id == "" {
		return send()
	}
	key := sentKey{dev, id}
	s.mu.Lock()
	now := time.Now()
	for k, t := range s.sent {
		if now.Sub(t.at) > sentTTL {
			delete(s.sent, k)
		}
	}
	if t, ok := s.sent[key]; ok {
		s.mu.Unlock()
		<-t.done
		result, _ := t.result.(T)
		return result, t.err
	}
	t := &sentTurn{done: make(chan struct{}), at: now}
	s.sent[key] = t
	s.mu.Unlock()

	result, err := send()
	t.result, t.err = result, err
	if err != nil {
		// A refusal is not remembered: the phone may fix the cause and send again.
		s.mu.Lock()
		delete(s.sent, key)
		s.mu.Unlock()
	}
	close(t.done)
	return result, err
}
