package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentcatalog"
	"github.com/repogoapp/host/internal/agents"
	"github.com/repogoapp/host/internal/agents/claude"
	"github.com/repogoapp/host/internal/agents/codex"
	"github.com/repogoapp/host/internal/agentusage"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/notify"
	"github.com/repogoapp/host/internal/testwait"
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
	// Claude, Codex and ACP run turns and keep history; ACP has no CLI to install,
	// sign in, or read models, usage and hooks from.
	if len(a.Agents) != 3 || len(a.Adapters) != 3 || len(a.Sessions) != 3 || len(a.Providers) != 2 || len(a.Hooks) != 2|| len(a.Catalogs) != 2 || len(a.Usage) != 2 || len(a.MCP) != 2 || len(a.Shipping) != 2 {
		t.Fatalf("incomplete registry: %+v", a)
	}
	for i, p := range a.Tools() {
		if agent.Kind(p.Kind) != a.Agents[i].Kind() || p.Name == "" || p.Category != clitool.CategoryAgent {
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
	if got := agentcatalog.New(discard(), r.Catalogs...).One(t.Context(), "direct", agentcatalog.Install{}, false); got.Available || got.Agent != "direct" {
		t.Fatalf("unsupported catalog: %+v", got)
	}
	if got := agentusage.New(r.Usage...).One(t.Context(), "direct"); got.Available || got.Agent != "direct" {
		t.Fatalf("unsupported usage: %+v", got)
	}
	if _, err := agentusage.New(r.Usage...).Reset(t.Context(), "direct", ""); !errors.Is(err, agentusage.ErrNoResets) {
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

func (p *catalogProvider) Catalog(context.Context) (agentcatalog.Catalog, error) {
	p.calls++
	if !p.available {
		return agentcatalog.Catalog{}, errors.New("fixture did not answer")
	}
	return agentcatalog.Catalog{Available: true}, nil
}

func TestCatalogRetriesFailureCachesSuccessAndForgets(t *testing.T) {
	p := &catalogProvider{}
	s := agentcatalog.New(discard(), p)
	s.One(t.Context(), "fixture", agentcatalog.Install{}, false)
	s.One(t.Context(), "fixture", agentcatalog.Install{}, false)
	if p.calls != 2 {
		t.Fatal("failure cached")
	}
	p.available = true
	s.One(t.Context(), "fixture", agentcatalog.Install{}, false)
	s.One(t.Context(), "fixture", agentcatalog.Install{}, false)
	if p.calls != 3 {
		t.Fatal("success not cached")
	}
	s.One(t.Context(), "fixture", agentcatalog.Install{}, true)
	if p.calls != 4 {
		t.Fatal("refresh read the cache")
	}
}

func TestCatalogReadsAgainForAnotherInstall(t *testing.T) {
	p := &catalogProvider{available: true}
	s := agentcatalog.New(discard(), p)
	s.Warm(t.Context(), []agent.Kind{"fixture"})
	v1 := agentcatalog.Install{Version: "1.0.0", Account: "a"}
	s.One(t.Context(), "fixture", v1, false)
	if p.calls != 1 {
		t.Fatal("the warmed catalog was not kept for the first install asked with")
	}
	s.One(t.Context(), "fixture", agentcatalog.Install{Version: "1.1.0", Account: "a"}, false)
	if p.calls != 2 {
		t.Fatal("an update kept the old catalog")
	}
	s.One(t.Context(), "fixture", agentcatalog.Install{Version: "1.1.0", Account: "b"}, false)
	if p.calls != 3 {
		t.Fatal("another account kept the old catalog")
	}
	s.One(t.Context(), "fixture", agentcatalog.Install{Version: "1.1.0", Account: "b"}, false)
	if p.calls != 3 {
		t.Fatal("the same install read again")
	}
}
