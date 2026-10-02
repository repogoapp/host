// Package ai writes chat titles and turn summaries with the local aigen model
// (a fine-tuned Qwen2.5-0.5B GGUF) run by a llama-server sidecar, both shipped
// inside the binary when it is built with -tags aigen.
package ai

import (
	"context"
	"errors"
	"sync"

	"github.com/repogo/host/internal/errkind"
)

var (
	// ErrNotBundled means this binary was built without -tags aigen.
	ErrNotBundled = errkind.New(errkind.Unavailable, "ai: model not bundled in this build")
	// ErrInvalid means the model answered but failed the format check.
	ErrInvalid = errors.New("ai: model output failed validation")
)

// Model runs the bundled model on first use and keeps the sidecar until Close.
// Any error from Title or TurnSummary means: fall back to the API model.
type Model struct {
	dir string
	cpu bool

	mu     sync.Mutex
	srv    *server
	client *client
}

// New keeps the unpacked server and model under dir. cpu turns GPU offload off.
func New(dir string, cpu bool) *Model {
	return &Model{dir: dir, cpu: cpu}
}

// Title names a chat from its first user message and, when there is one, the
// agent's first reply.
func (m *Model) Title(ctx context.Context, firstUser, reply string) (Result, error) {
	c, err := m.start(ctx)
	if err != nil {
		return Result{}, err
	}
	return check(c.title(ctx, firstUser, reply))
}

// TurnSummary updates previous (empty on the first turn) with the latest
// exchange: "You asked to X. <where it stands>."
func (m *Model) TurnSummary(ctx context.Context, previous, user, reply string) (Result, error) {
	c, err := m.start(ctx)
	if err != nil {
		return Result{}, err
	}
	exchange := renderTranscript([]turn{{"user", user}, {"assistant", reply}})
	if previous == "" {
		// The model was trained to rebuild a note from a transcript, not to
		// fold into an empty one.
		return check(c.rebuild(ctx, exchange))
	}
	return check(c.fold(ctx, previous, exchange))
}

// Close stops the sidecar if it started.
func (m *Model) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.srv == nil {
		return nil
	}
	err := m.srv.close()
	m.srv, m.client = nil, nil
	return err
}

func (m *Model) start(ctx context.Context) (*client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != nil && m.srv.alive() {
		return m.client, nil
	}
	p, err := extract(m.dir)
	if err != nil {
		return nil, err
	}
	srv, err := startServer(ctx, serverOptions{binary: p.server, model: p.model, cpu: m.cpu})
	if err != nil {
		return nil, err
	}
	m.srv, m.client = srv, &client{baseURL: srv.baseURL}
	return m.client, nil
}

func check(r Result, err error) (Result, error) {
	if err == nil && !r.Valid {
		err = ErrInvalid
	}
	return r, err
}
