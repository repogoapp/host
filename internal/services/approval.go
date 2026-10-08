package services

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/errkind"
)

func init() {
	emit.Register(Request{})
}

// ApprovalTimeout is how long a start waits for a device to approve it; no
// answer by then is a deny.
const ApprovalTimeout = 2 * time.Minute

const (
	StatePending = "pending"
	StateDone    = "done"
)

var ErrNotFound = errkind.New(errkind.NotFound, "no such services request")

// Request is a start of a project's services waiting for a device to approve
// it, sent to every device; sent again with State done once it is answered or
// lapses, so every phone drops its prompt.
type Request struct {
	RequestID string   `json:"request_id"`
	HostLabel string   `json:"host_label"`
	Path      string   `json:"path"`
	Services  []string `json:"services" wire:"array"`
	ExpiresAt string   `json:"expires_at"` // RFC 3339
	State     string   `json:"state"`
}

func (Request) Method() string { return "services.request" }

type asking struct {
	req Request
	// Buffered: Answer hands over the decision without waiting on this side.
	answer chan bool
}

// approve asks every device to start root's services and waits for the first
// answer. A deny, the timeout, or the host stopping skips the start.
func (m *Manager) approve(ctx context.Context, root string, services []serviceConfig) bool {
	var names []string
	for _, service := range services {
		if service.AutoStart {
			names = append(names, service.Name)
		}
	}
	if len(names) == 0 {
		return false
	}
	a := &asking{
		req: Request{
			RequestID: uuid.NewString(), HostLabel: m.deps.Label, Path: root, Services: names,
			ExpiresAt: time.Now().Add(m.approvalTimeout).UTC().Format(time.RFC3339),
			State:     StatePending,
		},
		answer: make(chan bool, 1),
	}
	m.mu.Lock()
	m.asks[a.req.RequestID] = a
	m.mu.Unlock()
	reached := m.deps.Ask(a.req)
	m.deps.Log.Info("services: start request sent", "request", a.req.RequestID, "root", root, "services", names, "devices", reached)

	timer := time.NewTimer(m.approvalTimeout)
	defer timer.Stop()
	select {
	case approved := <-a.answer:
		return approved
	case <-timer.C:
	case <-ctx.Done():
	}
	if _, ok := m.take(a.req.RequestID); ok {
		m.deps.Log.Info("services: start request lapsed; not starting", "request", a.req.RequestID, "root", root)
		m.done(a.req)
		return false
	}
	// Answer won the race and is handing the decision over.
	return <-a.answer
}

// Pending is every start request still waiting, oldest first.
func (m *Manager) Pending() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, 0, len(m.asks))
	for _, a := range m.asks {
		out = append(out, a.req)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt < out[j].ExpiresAt })
	return out
}

// Answer approves or denies a start request.
func (m *Manager) Answer(id string, approved bool) error {
	a, ok := m.take(id)
	if !ok {
		return ErrNotFound
	}
	m.deps.Log.Info("services: start request answered", "request", id, "approved", approved)
	a.answer <- approved
	m.done(a.req)
	return nil
}

func (m *Manager) take(id string) (*asking, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.asks[id]
	delete(m.asks, id)
	return a, ok
}

func (m *Manager) done(req Request) {
	req.State = StateDone
	m.deps.Ask(req)
}
