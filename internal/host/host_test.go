package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents"
	"github.com/repogo/host/internal/chatlive"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/host"
	"github.com/repogo/host/internal/power"
	"github.com/repogo/host/internal/rpc/registry"
	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/testhost"
	"github.com/repogo/host/internal/testwait"
)

// turnAdapter opens session s1, says "hi" once told to, and runs until stopped.
type turnAdapter struct{ opened, speak chan struct{} }

func (turnAdapter) Kind() agent.Kind { return agent.KindClaude }
func (turnAdapter) Available() error { return nil }
func (a turnAdapter) Send(ctx context.Context, _ agent.TurnRequest, io agent.TurnIO) (agent.Result, error) {
	io.Session("s1")
	close(a.opened)
	<-a.speak
	io.Emit(agent.Event{Kind: agent.EventText, Text: "hi"})
	<-ctx.Done()
	return agent.Result{}, ctx.Err()
}

// withAdapter registers a in place of the providers' own adapters.
func withAdapter(a agent.Adapter) func(*host.Config) {
	return func(c *host.Config) {
		c.Agents = func(deps agent.Dependencies) (*agents.Registry, error) {
			r, err := agents.New(deps)
			if r != nil {
				r.Adapters = []agent.Adapter{a}
			}
			return r, err
		}
	}
}

// pushes records what the host sends one device.
type pushes chan string

func (p pushes) Send(_ device.ID, method string, payload []byte) error {
	select {
	case p <- method + " " + string(payload):
	default:
	}
	return nil
}

// A turn the host runs reaches the chat's status, the live stream and the
// queue (state.db, and the row's count and version) through the wiring
// production uses.
func TestManagedTurnsReachStatusStreamAndQueue(t *testing.T) {
	a := turnAdapter{opened: make(chan struct{}), speak: make(chan struct{})}
	th := testhost.New(t, withAdapter(a))
	req := agent.TurnRequest{ChatID: "claude:s1", SessionID: "s1", Cwd: th.Root, Agent: agent.KindClaude, Prompt: "one"}
	first, err := th.Services.Runner.Send(req)
	if err != nil {
		t.Fatal(err)
	}
	settled := func(turnID string) func() bool {
		return func() bool {
			st, err := th.Services.Runner.Status(turnID)
			return err != nil || (st.State != agent.StateQueued && st.State != agent.StateRunning)
		}
	}
	t.Cleanup(func() {
		th.Services.Runner.Stop(first.TurnID)
		testwait.For(t, "the turn to settle", settled(first.TurnID))
	})

	if _, err := th.Services.Store.Info("claude:s1"); err != nil {
		t.Fatalf("no chat status for a running turn: %v", err)
	}

	req.Prompt = "two"
	second, err := th.Services.Runner.Send(req)
	if err != nil {
		t.Fatal(err)
	}
	q, err := th.Services.Store.Queue("claude:s1")
	if err != nil || len(q.Queued) != 1 || q.Queued[0].TurnID != second.TurnID || q.Queued[0].Prompt != "two" {
		t.Fatalf("queue = %+v (%v), want %s", q, err, second.TurnID)
	}
	if row, _ := th.Services.Store.Info("claude:s1"); row.QueuedCount != 1 || row.QueueRev != q.QueueRev {
		t.Fatalf("row queue = %d at %d, want 1 at %d", row.QueuedCount, row.QueueRev, q.QueueRev)
	}
	if err := th.Services.Runner.RemoveQueued(second.TurnID); err != nil {
		t.Fatal(err)
	}
	if row, _ := th.Services.Store.Info("claude:s1"); row.QueuedCount != 0 || row.QueueRev <= q.QueueRev {
		t.Fatalf("row queue after removing = %d at %d, want 0 past %d", row.QueuedCount, row.QueueRev, q.QueueRev)
	}

	<-a.opened
	got := make(pushes, 64)
	th.Pushes.Attach("phone", got)
	if _, err := th.Services.Live.Subscribe("phone", "claude:s1", 0); err != nil {
		t.Fatal(err)
	}
	close(a.speak)
	deadline := time.After(testwait.Timeout)
	for {
		select {
		case p := <-got:
			if strings.HasPrefix(p, chatlive.Streaming{}.Method()+" ") && strings.Contains(p, `"text":"hi"`) {
				return
			}
		case <-deadline:
			t.Fatal("the turn's text never streamed")
		}
	}
}

// A hook the agent drops reaches the chat's status.
func TestHookDropsReachStatus(t *testing.T) {
	var state string
	th := testhost.New(t, func(c *host.Config) { state = c.State })
	at := time.Now().UnixMilli()
	payload, err := json.Marshal(map[string]any{
		"agent": "claude", "event": "UserPromptSubmit", "origin": "cli", "at_ms": at,
		"payload": map[string]string{"session_id": "s2", "cwd": th.Root, "prompt_id": "p1", "prompt": "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	drop := filepath.Join(state, "notify", fmt.Sprintf("%d-1.json", at))
	if err := os.WriteFile(drop, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	th.Start()

	testwait.For(t, "the hook in the chat's status", func() bool {
		_, err := th.Services.Store.Info("claude:s2")
		return err == nil
	})
}

// probe is a provider holding a resource and a power hold, each counting releases.
type probe struct{ closed, released atomic.Int32 }

func (*probe) Kind() agent.Kind                 { return "probe" }
func (*probe) Name() string                     { return "Probe" }
func (p *probe) Close()                         { p.closed.Add(1) }
func (*probe) Battery() (power.Battery, bool)   { return power.Battery{}, false }
func (*probe) Run(context.Context)              {}
func (p *probe) Release()                       { p.released.Add(1) }
func (p *probe) counts() (closed, released int) { return int(p.closed.Load()), int(p.released.Load()) }

func probed(t *testing.T) (host.Config, *probe) {
	cfg, p := testhost.Config(t), &probe{}
	build := cfg.Agents
	cfg.Agents = func(deps agent.Dependencies) (*agents.Registry, error) {
		r, err := build(deps)
		if r != nil {
			r.Agents = append(r.Agents, p)
		}
		return r, err
	}
	cfg.Power = func(*slog.Logger, func() bool, func(power.Battery)) (host.Keeper, error) { return p, nil }
	return cfg, p
}

// A host that fails partway releases what it acquired, once.
func TestFailedNewReleasesWhatItAcquired(t *testing.T) {
	cfg, p := probed(t)
	if err := os.WriteFile(filepath.Join(cfg.State, "env-sources.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.New(t.Context(), cfg); err == nil {
		t.Fatal("New succeeded over an unreadable env-sources.json")
	}
	if closed, released := p.counts(); closed != 1 || released != 1 {
		t.Fatalf("closed %d, released %d; want each once", closed, released)
	}
}

// A host whose port is taken fails to listen; closing it, twice, releases
// everything once and closes its storage.
func TestCloseAfterAFailedListenIsSafeTwice(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	cfg, p := probed(t)
	cfg.Port = taken.Addr().(*net.TCPAddr).Port
	h, err := host.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Listen(); err == nil {
		t.Fatal("Listen succeeded on a taken port")
	}
	h.Close()
	h.Close()
	if closed, released := p.counts(); closed != 1 || released != 1 {
		t.Fatalf("closed %d, released %d; want each once", closed, released)
	}
	if _, err := h.Services.Store.Info("claude:none"); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("chat cache still open after Close: %v", err)
	}
}

// Close cancels the host's workers and waits for them before it closes the
// storage they use.
func TestCloseStopsWorkersBeforeStorage(t *testing.T) {
	th := testhost.New(t)
	th.Start()
	cancelled, release, used := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	th.Spawn(func(ctx context.Context) {
		<-ctx.Done()
		close(cancelled)
		<-release
		_, err := th.Services.Store.Info("claude:none")
		if errors.Is(err, store.ErrNotFound) {
			err = nil
		}
		used <- err
	})

	closed := make(chan struct{})
	go func() {
		th.Close()
		close(closed)
	}()
	<-cancelled
	select {
	case <-closed:
		t.Fatal("Close returned before its worker exited")
	default:
	}
	close(release)
	<-closed
	if err := <-used; err != nil {
		t.Fatalf("storage closed under a running worker: %v", err)
	}
}

// A config or registry missing a dependency fails construction, naming it.
func TestMissingDependenciesAreNamed(t *testing.T) {
	cfg := testhost.Config(t)
	cfg.Agents, cfg.Log = nil, nil
	if _, err := host.New(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "Agents, ") || !strings.HasSuffix(err.Error(), "Log") {
		t.Fatalf("New = %v, want Agents and Log named", err)
	}
	if _, err := registry.New(registry.Config{}); err == nil || !strings.Contains(err.Error(), "Chats") {
		t.Fatalf("registry.New = %v, want Chats named", err)
	}
}
