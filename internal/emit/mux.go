package emit

import (
	"errors"
	"sync"

	"github.com/repogoapp/host/internal/device"
)

// ErrNoRoute is returned when no transport can reach a device: none has
// claimed it.
var ErrNoRoute = errors.New("emit: no route to device")

// Mux is the Transport producers hold: a push goes out on whichever transport
// attached the device, its socket on loopback or through the relay.
type Mux struct {
	mu     sync.RWMutex
	routes map[device.ID]Transport
}

func NewMux() *Mux {
	return &Mux{routes: map[device.ID]Transport{}}
}

// Attach claims a device for a transport. A later attach replaces an earlier
// one: the newest connection is the one the device is actually on.
func (m *Mux) Attach(d device.ID, t Transport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes[d] = t
}

// Detach releases a device, but only if t still holds it. A connection that
// closes after its successor attached must not take the successor's route.
func (m *Mux) Detach(d device.ID, t Transport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.routes[d] == t {
		delete(m.routes, d)
	}
}

// Attached is every device a transport has claimed: the clients with a
// socket on this host, on loopback or through the relay.
func (m *Mux) Attached() []device.ID {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]device.ID, 0, len(m.routes))
	for id := range m.routes {
		ids = append(ids, id)
	}
	return ids
}

// Send delivers to the device's attached transport.
func (m *Mux) Send(to device.ID, method string, payload []byte) error {
	m.mu.RLock()
	t := m.routes[to]
	m.mu.RUnlock()
	if t == nil {
		return ErrNoRoute
	}
	return t.Send(to, method, payload)
}
