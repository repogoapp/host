package emit

import (
	"errors"
	"sync"

	"github.com/repogo/host/internal/device"
)

// ErrNoRoute is returned when no transport can reach a device: none has
// claimed it and there is no fallback.
var ErrNoRoute = errors.New("emit: no route to device")

// Mux is the Transport producers hold: a push goes out on whichever transport
// attached the device, else the fallback, the relay, whose sessions this host cannot see.
type Mux struct {
	mu       sync.RWMutex
	routes   map[device.ID]Transport
	fallback Transport
}

func NewMux() *Mux {
	return &Mux{routes: map[device.ID]Transport{}}
}

// SetFallback is where a device is sent when nothing has attached for it.
func (m *Mux) SetFallback(t Transport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallback = t
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

// Send delivers to the device's attached transport, else the fallback.
func (m *Mux) Send(to device.ID, method string, payload []byte) error {
	m.mu.RLock()
	t, ok := m.routes[to]
	if !ok {
		t = m.fallback
	}
	m.mu.RUnlock()
	if t == nil {
		return ErrNoRoute
	}
	return t.Send(to, method, payload)
}
