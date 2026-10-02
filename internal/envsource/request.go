package envsource

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/repogo/host/internal/emit"
)

func init() {
	emit.Register(Request{})
}

// RequestTimeout is how long a start waits for the phone before it goes ahead
// without the variables, as v1 did.
const RequestTimeout = 2 * time.Minute

const (
	StatePending = "pending"
	StateDone    = "done"
)

// Request is a start on this host waiting for secrets bound on another
// machine, sent to every device; sent again with State done once it is
// answered or lapses, so every phone drops its sheet.
type Request struct {
	RequestID string   `json:"request_id"`
	HostLabel string   `json:"host_label"`
	Path      string   `json:"path"`
	Handles   []string `json:"handles" wire:"array"`
	Services  []string `json:"services" wire:"array"`
	ExpiresAt string   `json:"expires_at"` // RFC 3339
	State     string   `json:"state"`
}

func (Request) Method() string { return "env.request" }

// Requests brokers envFrom handles for this host's starts: its own bindings
// directly, anything else through a phone, which reads it where it is bound.
type Requests struct {
	sources *Sources
	label   string
	// send delivers a request to every device and says how many it reached;
	// alert wakes the phones when that was none.
	send    func(Request) int
	alert   func(Request)
	timeout time.Duration
	log     *slog.Logger

	mu      sync.Mutex
	pending map[string]*waiting
}

type waiting struct {
	req    Request
	answer chan map[string]map[string]string
}

func NewRequests(sources *Sources, label string, send func(Request) int, alert func(Request), log *slog.Logger) *Requests {
	return &Requests{
		sources: sources, label: label, send: send, alert: alert,
		timeout: RequestTimeout, log: log,
		pending: map[string]*waiting{},
	}
}

// Resolve returns the variables it could get, keyed by handle. Local handles
// are read at once (approval guards another machine's file); the rest wait for
// one phone answer or the timeout, and the start goes ahead either way.
func (r *Requests) Resolve(ctx context.Context, path string, handles, services []string) map[string]map[string]string {
	out := map[string]map[string]string{}
	var remote []string
	for handle, res := range r.sources.Read(handles) {
		switch {
		case res.OK:
			out[handle] = res.Vars
		case res.Error == NotBound:
			remote = append(remote, handle)
		default:
			r.log.Warn("env source unreadable; starting without it", "handle", handle, "error", res.Error)
		}
	}
	if len(remote) == 0 {
		r.log.Info("env request: all bound here", "path", path, "handles", len(handles), "read", len(out))
		return out
	}
	sort.Strings(remote)

	w := &waiting{
		req: Request{
			RequestID: uuid.NewString(), HostLabel: r.label, Path: path,
			Handles: remote, Services: append([]string{}, services...),
			ExpiresAt: time.Now().Add(r.timeout).UTC().Format(time.RFC3339),
			State:     StatePending,
		},
		// Buffered: Provide hands over the answer without waiting on this side.
		answer: make(chan map[string]map[string]string, 1),
	}
	r.mu.Lock()
	r.pending[w.req.RequestID] = w
	r.mu.Unlock()
	reached := r.send(w.req)
	r.log.Info("env request: sent", "request", w.req.RequestID, "path", path,
		"handles", remote, "services", services, "devices", reached)
	if reached == 0 {
		r.alert(w.req)
	}

	timer := time.NewTimer(r.timeout)
	defer timer.Stop()
	var got map[string]map[string]string
	select {
	case got = <-w.answer:
	case <-timer.C:
	case <-ctx.Done():
	}
	if got == nil {
		if _, ok := r.take(w.req.RequestID); ok {
			r.log.Info("env request lapsed; starting without its variables", "request", w.req.RequestID)
			r.done(w.req)
		} else {
			// Provide won the race and is handing the answer over.
			got = <-w.answer
		}
	}
	for handle, vars := range got {
		out[handle] = vars
	}
	return out
}

// Pending is every request still waiting, oldest first.
func (r *Requests) Pending() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Request, 0, len(r.pending))
	for _, w := range r.pending {
		out = append(out, w.req)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt < out[j].ExpiresAt })
	return out
}

// Provide answers a request: the variables of each handle it asked for that
// the phone could read, or none when denied.
func (r *Requests) Provide(id string, approved bool, results map[string]ReadResult) error {
	w, ok := r.take(id)
	if !ok {
		return ErrNotFound
	}
	vars := map[string]map[string]string{}
	if approved {
		for _, handle := range w.req.Handles {
			if res, ok := results[handle]; ok && res.OK && res.Vars != nil {
				vars[handle] = res.Vars
			}
		}
	}
	r.log.Info("env request: answered", "request", id, "approved", approved, "provided", len(vars), "asked", len(w.req.Handles))
	w.answer <- vars
	r.done(w.req)
	return nil
}

func (r *Requests) take(id string) (*waiting, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.pending[id]
	delete(r.pending, id)
	return w, ok
}

func (r *Requests) done(req Request) {
	req.State = StateDone
	r.send(req)
}
