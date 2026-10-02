package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agentcatalog"
	"github.com/repogo/host/internal/agents"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/agentusage"
	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/testwait"
)

func TestRegistryConstructsIsolatedProviders(t *testing.T) {
	a, err := agents.New(agent.Dependencies{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := agents.New(agent.Dependencies{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if len(a.Agents) != 3 || len(a.Adapters) != 3 || len(a.Sessions) != 3 || len(a.Hooks) != 2 || len(a.Catalogs) != 3 || len(a.Usage) != 3 || len(a.MCP) != 2 || len(a.Shipping) != 2 {
		t.Fatalf("incomplete registry: %+v", a)
	}
	for i, p := range a.Tools() {
		if agent.Kind(p.Kind) != a.Agents[i].Kind() || p.Name == "" || p.Icon == "" || p.Category != clitool.CategoryAgent {
			t.Fatalf("bad identity: %+v", p)
		}
		if !slices.Contains(p.Capabilities, agent.CapabilityRun) || !slices.Contains(p.Capabilities, clitool.CapabilityLogin) {
			t.Fatalf("missing capabilities: %v", p.Capabilities)
		}
		if a.Adapters[i] == b.Adapters[i] || a.Sessions[i] == b.Sessions[i] || p.Home == b.Tools()[i].Home {
			t.Fatal("provider state shared between hosts")
		}
	}
	if _, ok := a.Usage[0].(agentusage.Resetter); ok {
		t.Fatal("Claude advertises resets")
	}
	if _, ok := a.Usage[1].(agentusage.Resetter); !ok {
		t.Fatal("Codex lost resets")
	}
	if _, ok := a.Usage[2].(agentusage.Resetter); ok {
		t.Fatal("Cursor advertises resets")
	}
	for i, p := range a.Shipping {
		if got := filepath.Dir(p.UsageRoots()[0]); got != a.Tools()[i].Home {
			t.Fatalf("home escaped instance: %s", got)
		}
	}
}

// No CLI process, catalog, hooks, or session reader is needed for turns.
type directProvider struct {
	answered chan json.RawMessage
	stopped  chan struct{}
}

func (*directProvider) Kind() agent.Kind { return "direct" }
func (*directProvider) Name() string     { return "Direct" }
func (*directProvider) Available() error { return nil }

func (p *directProvider) Send(ctx context.Context, req agent.TurnRequest, stream agent.TurnIO) (agent.Result, error) {
	if req.Prompt == "stop" {
		<-ctx.Done()
		close(p.stopped)
		return agent.Result{}, ctx.Err()
	}
	answer, err := stream.Ask(ctx, agent.Approval{CallID: "approve", Title: "Proceed?"})
	if err != nil {
		return agent.Result{}, err
	}
	p.answered <- answer
	stream.Emit(agent.Event{Kind: agent.EventText, Text: "done"})
	return agent.Result{}, nil
}

func TestDirectProviderApprovalStopAndAbsentCapabilities(t *testing.T) {
	p := &directProvider{answered: make(chan json.RawMessage, 1), stopped: make(chan struct{})}
	r, err := agents.Construct(agent.Dependencies{}, func(agent.Dependencies) agent.Identity { return p })
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.Adapters) != 1 || len(r.Catalogs)+len(r.Usage)+len(r.Sessions)+len(r.Hooks)+len(r.MCP) != 0 {
		t.Fatal("absent capabilities were synthesized")
	}
	if got := agentcatalog.New(r.Kinds(), r.Catalogs...).One(t.Context(), "direct", false); got.Available || got.Detail != "model discovery is not supported" {
		t.Fatalf("unsupported catalog: %+v", got)
	}
	if got := agentcatalog.New(r.Kinds(), r.Catalogs...).One(t.Context(), "missing", false); got.Detail != "unknown agent" {
		t.Fatalf("unknown catalog: %+v", got)
	}
	if got := agentusage.New(r.Kinds(), r.Usage...).One(t.Context(), "direct"); got.Detail != "usage reporting is not supported" {
		t.Fatalf("unsupported usage: %+v", got)
	}
	if got := agentusage.New(r.Kinds(), r.Usage...).One(t.Context(), "missing"); got.Detail != "unknown agent" {
		t.Fatalf("unknown usage: %+v", got)
	}
	if _, err := agentusage.New(r.Kinds(), r.Usage...).Reset(t.Context(), "direct", ""); !errors.Is(err, agentusage.ErrNoResets) {
		t.Fatalf("reset: %v", err)
	}
	mgr := agent.NewManager(discard(), agent.Hooks{
		Session: func(agent.TurnStatus) {}, Status: func(agent.TurnStatus, agent.ChatStatus, time.Time) {},
		SaveQueue: func(string, []agent.StoredTurn) error { return nil },
	}, r.Adapters...)
	st, err := mgr.Send(agent.TurnRequest{Agent: p.Kind(), ChatID: "direct:one", Cwd: t.TempDir(), Prompt: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop(st.TurnID)
	testwait.For(t, "the approval", func() bool { got, _ := mgr.Status(st.TurnID); return got.Approval != nil })
	if err := mgr.Respond(st.TurnID, "approve", json.RawMessage(`"allow"`)); err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-p.answered:
		if string(answer) != `"allow"` {
			t.Fatalf("answer %s", answer)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approval not delivered")
	}
	st, err = mgr.Send(agent.TurnRequest{Agent: p.Kind(), ChatID: "direct:two", Cwd: t.TempDir(), Prompt: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Stop(st.TurnID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation not delivered")
	}
}

func TestRegistryRejectsDuplicateKinds(t *testing.T) {
	factory := func(agent.Dependencies) agent.Identity { return &directProvider{} }
	if _, err := agents.Construct(agent.Dependencies{}, factory, factory); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestProviderHookSemantics(t *testing.T) {
	cases := []struct {
		hooks                     notify.Provider
		event, notification, tool string
		want                      agent.ChatStatus
	}{
		{claude.Hooks{}, "UserPromptSubmit", "", "", agent.ChatWorking},
		{claude.Hooks{}, "PermissionRequest", "", "AskUserQuestion", agent.ChatAwaitingUser},
		{claude.Hooks{}, "PermissionRequest", "", "Bash", agent.ChatAwaitingApproval},
		{claude.Hooks{}, "Stop", "", "", agent.ChatCompleted},
		{claude.Hooks{}, "StopFailure", "", "", agent.ChatFailed},
		{codex.Hooks{}, "UserPromptSubmit", "", "", agent.ChatWorking},
		{codex.Hooks{}, "PermissionRequest", "", "Bash", agent.ChatAwaitingApproval},
		{codex.Hooks{}, "Stop", "", "", agent.ChatCompleted},
		{codex.Hooks{}, "Interrupt", "", "", agent.ChatInterrupted},
		{codex.Hooks{}, "unknown", "", "", agent.ChatUnknown},
	}
	for _, c := range cases {
		if got := c.hooks.HookStatus(c.event, c.notification, c.tool); got != c.want {
			t.Errorf("%T %s: %s", c.hooks, c.event, got)
		}
	}
}

type catalogProvider struct {
	calls     int
	available bool
}

func (*catalogProvider) Kind() agent.Kind { return "fixture" }

func (p *catalogProvider) Catalog(context.Context) agentcatalog.Catalog {
	p.calls++
	return agentcatalog.Catalog{Available: p.available}
}

func TestCatalogRetriesFailureCachesSuccessAndForgets(t *testing.T) {
	p := &catalogProvider{}
	s := agentcatalog.New([]agent.Kind{"fixture"}, p)
	s.One(t.Context(), "fixture", false)
	s.One(t.Context(), "fixture", false)
	if p.calls != 2 {
		t.Fatal("failure cached")
	}
	p.available = true
	s.One(t.Context(), "fixture", false)
	s.One(t.Context(), "fixture", false)
	if p.calls != 3 {
		t.Fatal("success not cached")
	}
	s.One(t.Context(), "fixture", true)
	if p.calls != 4 {
		t.Fatal("refresh read the cache")
	}
	if got := s.All(t.Context(), false); !reflect.DeepEqual(got, []agentcatalog.Catalog{{Agent: "fixture", Available: true}}) {
		t.Fatalf("all: %+v", got)
	}
}
