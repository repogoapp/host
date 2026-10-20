package catalog_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentcatalog"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/files"
	"github.com/repogoapp/host/internal/hostsetup"
	mcpcore "github.com/repogoapp/host/internal/mcp"
	pluginscore "github.com/repogoapp/host/internal/plugins"
	"github.com/repogoapp/host/internal/projectagent"
	"github.com/repogoapp/host/internal/rpc"
	catalogrpc "github.com/repogoapp/host/internal/rpc/catalog"
)

type readResult struct {
	Text string `json:"text"`
}

// TestEnvironmentReadListsFromHome is the environment alone: its agents, and
// the plugins and servers read at home, with no project agents or host tools.
func TestEnvironmentReadListsFromHome(t *testing.T) {
	f := newFixture(t)
	text := f.read(t, map[string]any{})
	for _, want := range []string{"- Claude (claude): ready. Models: Opus (opus).", "plugin:plans.list()", "mcp:linear.get_issue(id)"} {
		if !strings.Contains(text, want) {
			t.Errorf("read missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"Project agents:", "RepoGo:"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("an environment's read has %q:\n%s", unwanted, text)
		}
	}
	if f.plugins.path() != f.home || f.mcp.path() != f.home {
		t.Errorf("read from %q and %q, want home %q", f.plugins.path(), f.mcp.path(), f.home)
	}
}

// TestProjectReadAddsItsAgentsAndHostTools reads the project's folder, its
// own agents and the router's Tool methods.
func TestProjectReadAddsItsAgentsAndHostTools(t *testing.T) {
	f := newFixture(t)
	text := f.read(t, map[string]any{"project": f.project})
	for _, want := range []string{"Project agents:\n- Helper (helper): ready.", "RepoGo:\n- repogo:t.status(): A test read."} {
		if !strings.Contains(text, want) {
			t.Errorf("read missing %q:\n%s", want, text)
		}
	}
	if f.plugins.path() != f.project || f.mcp.path() != f.project {
		t.Errorf("read from %q and %q, want the project %q", f.plugins.path(), f.mcp.path(), f.project)
	}
}

func TestProjectOutsideTheRootsIsRefused(t *testing.T) {
	f := newFixture(t)
	if _, err := f.call(t, map[string]any{"project": t.TempDir()}); err == nil {
		t.Fatal("read of a folder outside the roots succeeded")
	}
}

// TestLatePartShowsAsMissing holds the MCP servers past the wait: the read
// answers with the rest and says the servers didn't answer.
func TestLatePartShowsAsMissing(t *testing.T) {
	f := newFixture(t)
	f.mcp.hold = make(chan struct{})
	defer close(f.mcp.hold)
	text := f.read(t, map[string]any{})
	if !strings.Contains(text, "MCP servers:\n- Couldn't list them: it didn't answer in time") {
		t.Errorf("late servers not reported:\n%s", text)
	}
	if !strings.Contains(text, "plugin:plans.list()") {
		t.Errorf("the plugins went missing with the servers:\n%s", text)
	}
}

// TestNamedReadAsksOnlyItsPart answers one server in full without listing
// plugins or probing agents.
func TestNamedReadAsksOnlyItsPart(t *testing.T) {
	f := newFixture(t)
	text := f.read(t, map[string]any{"project": f.project, "name": "mcp:linear"})
	if !strings.HasPrefix(text, "Linear (mcp:linear):\n- mcp:linear.get_issue(id)") {
		t.Errorf("named read = %q", text)
	}
	if f.plugins.path() != "" || f.tools.listed {
		t.Errorf("named server read also listed plugins (%q) or probed agents (%v)", f.plugins.path(), f.tools.listed)
	}
	if _, err := f.call(t, map[string]any{"name": "mcp:nope"}); err == nil {
		t.Error("read of an unknown server succeeded")
	}
}

type fixture struct {
	router  *rpc.Router
	home    string
	project string
	plugins *fakePlugins
	mcp     *fakeMCP
	tools   *fakeTools
}

func newFixture(t *testing.T) *fixture {
	// Contain answers the real path, past macOS's /var link.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		router: rpc.New(slog.New(slog.DiscardHandler)), home: t.TempDir(), project: root,
		plugins: &fakePlugins{}, mcp: &fakeMCP{}, tools: &fakeTools{},
	}
	rpc.Add(f.router, "t.status", func(context.Context, rpc.Caller, rpc.None) (rpc.Ack, error) { return rpc.OK, nil },
		rpc.Tool("A test read."), rpc.Read)
	catalogrpc.Register(f.router, catalogrpc.Deps{
		Files: files.New(files.Config{Roots: files.StaticRoots{root}}), Home: f.home,
		Setup:         hostsetup.Sources{Tools: f.tools, Catalog: fakeCatalog{}},
		ProjectAgents: fakeProjectAgents{}, Plugins: f.plugins, MCP: f.mcp, Wait: 200 * time.Millisecond,
	})
	return f
}

func (f *fixture) call(t *testing.T, params map[string]any) (readResult, error) {
	t.Helper()
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.router.Call(t.Context(), rpc.Caller{Device: "phone", Scope: rpc.ScopeRemote}, "catalog.read", b)
	if err != nil {
		return readResult{}, err
	}
	var result readResult
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result, nil
}

func (f *fixture) read(t *testing.T, params map[string]any) string {
	t.Helper()
	result, err := f.call(t, params)
	if err != nil {
		t.Fatal(err)
	}
	return result.Text
}

type fakePlugins struct {
	mu     sync.Mutex
	listed string
}

func (p *fakePlugins) Plugins(_ context.Context, path string) ([]pluginscore.Listed, error) {
	p.mu.Lock()
	p.listed = path
	p.mu.Unlock()
	return []pluginscore.Listed{{Name: "plans", Dir: "project", Methods: []pluginscore.ListedMethod{{Name: "plans.list", Kind: "query"}}}}, nil
}

func (p *fakePlugins) path() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listed
}

type fakeMCP struct {
	hold   chan struct{}
	mu     sync.Mutex
	listed string
}

func (m *fakeMCP) Tools(_ context.Context, cwd string, described bool) []mcpcore.ServerTools {
	if m.hold != nil {
		<-m.hold
	}
	m.mu.Lock()
	m.listed = cwd
	m.mu.Unlock()
	input := json.RawMessage(`{"properties":{"id":{}},"required":["id"]}`)
	if !described {
		input = nil
	}
	return []mcpcore.ServerTools{{Server: "linear", Label: "Linear", Tools: []mcpcore.ToolInfo{{Name: "get_issue", ReadOnly: true, Input: input}}}}
}

func (m *fakeMCP) path() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listed
}

type fakeTools struct {
	mu     sync.Mutex
	listed bool
}

func (f *fakeTools) List(context.Context) []clitool.Install {
	f.mu.Lock()
	f.listed = true
	f.mu.Unlock()
	return []clitool.Install{{Kind: "claude", Name: "Claude Code", Category: clitool.CategoryAgent, Installed: true, Authed: true}}
}

func (f *fakeTools) AccountKey(string) string { return "" }

type fakeCatalog struct{}

func (fakeCatalog) One(context.Context, agent.Kind, agentcatalog.Install, bool) agentcatalog.Catalog {
	return agentcatalog.Catalog{Name: "Claude", Models: []agentcatalog.Model{{ID: "opus", Name: "Opus"}}}
}

type fakeProjectAgents struct{}

func (fakeProjectAgents) ProjectAgents(string) ([]projectagent.ProjectAgent, error) {
	return []projectagent.ProjectAgent{{ID: "helper", Name: "Helper"}}, nil
}

func (fakeProjectAgents) ProjectAgentIcon(string, string) string { return "" }
