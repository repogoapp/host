package catalog_test

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/agentcatalog"
	"github.com/repogoapp/host/internal/catalog"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/hostsetup"
	"github.com/repogoapp/host/internal/mcp"
	"github.com/repogoapp/host/internal/plugins"
	"github.com/repogoapp/host/internal/projectagent"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/testhost"
)

var update = flag.Bool("update", false, "rewrite testdata from what the catalog renders")

const hint = "run: go test ./internal/catalog -update"

// TestRendersEachPart pins the text a voice session reads, one file per case.
func TestRendersEachPart(t *testing.T) {
	full := fixture()
	failed := catalog.Parts{
		AgentsErr:        errors.New("it didn't answer in time"),
		ProjectAgentsErr: errors.New("manifest unreadable"),
		PluginsErr:       errors.New("plugins: the runtime crashed"),
		ServersErr:       errors.New("it didn't answer in time"),
	}
	plugin, err := catalog.Detail(full, "plans")
	if err != nil {
		t.Fatal(err)
	}
	server, err := catalog.Detail(full, "mcp:vercel")
	if err != nil {
		t.Fatal(err)
	}
	testhost.GoldenDir(t, "testdata", map[string][]byte{
		"project.txt":     []byte(catalog.Render(full)),
		"environment.txt": []byte(catalog.Render(catalog.Parts{Agents: full.Agents, Plugins: full.Plugins[1:], Servers: full.Servers[:1]})),
		"failed.txt":      []byte(catalog.Render(failed)),
		"plugin.txt":      []byte(plugin),
		"server.txt":      []byte(server),
	}, *update, hint)
}

func TestDetailNamesWhatExists(t *testing.T) {
	full := fixture()
	if _, err := catalog.Detail(full, "plugin:nope"); !errors.Is(err, errkind.ErrNotFound) || !strings.Contains(err.Error(), "there are plans, vercel") {
		t.Errorf("Detail(plugin:nope) = %v, want not found naming plans, vercel", err)
	}
	if _, err := catalog.Detail(full, "mcp:nope"); !errors.Is(err, errkind.ErrNotFound) || !strings.Contains(err.Error(), "mcp:linear, mcp:vercel, mcp:stripe") {
		t.Errorf("Detail(mcp:nope) = %v, want not found naming the servers", err)
	}
	late := errors.New("it didn't answer in time")
	if _, err := catalog.Detail(catalog.Parts{ServersErr: late}, "mcp:linear"); !errors.Is(err, late) {
		t.Errorf("Detail with the servers late = %v, want the late error, not not found", err)
	}
	if _, err := catalog.Detail(catalog.Parts{PluginsErr: late}, "plans"); !errors.Is(err, late) {
		t.Errorf("Detail with the plugins late = %v, want the late error, not not found", err)
	}
}

func fixture() catalog.Parts {
	vercelTools := make([]mcp.ToolInfo, 20)
	for i := range vercelTools {
		vercelTools[i] = mcp.ToolInfo{Name: fmt.Sprintf("tool_%02d", i), ReadOnly: true, Description: "Reads Vercel.", Input: schema(`{"properties":{"team":{}}}`)}
	}
	return catalog.Parts{
		Agents: []hostsetup.Agent{
			{
				Install: clitool.Install{Kind: "claude", Name: "Claude Code", Installed: true, Authed: true},
				Catalog: agentcatalog.Catalog{Name: "Claude", DefaultModel: "opus", Models: []agentcatalog.Model{
					{ID: "opus", Name: "Opus"}, {ID: "sonnet", Name: "Sonnet"}, {ID: "old", Name: "Old", Hidden: true},
				}},
			},
			{Install: clitool.Install{Kind: "codex", Name: "Codex", Installed: true}, Catalog: agentcatalog.Catalog{Name: "Codex"}},
			{Install: clitool.Install{Kind: "opencode", Name: "OpenCode", Installed: true, Authed: true}, Catalog: agentcatalog.Catalog{Name: "OpenCode", Models: routerModels(14)}},
			{Install: clitool.Install{Kind: "gemini", Name: "Gemini CLI"}, Catalog: agentcatalog.Catalog{Name: "Gemini CLI"}},
		},
		ProjectAgents: []projectagent.ProjectAgent{
			{ID: "helper", Name: "Helper", Model: "claude-sonnet-5-5", Models: []projectagent.ProjectAgentModel{{ID: "claude-sonnet-5-5", Name: "Sonnet"}}},
			{ID: "grok", Name: "Grok", Error: "agent.json: no command"},
		},
		Plugins: []plugins.Listed{
			{Name: "plans", Dir: "project", Description: "Plans for this project. Kept in its data folder.", Methods: []plugins.ListedMethod{
				{Name: "plans.list", Kind: "query", Description: "Every plan."},
				{Name: "plans.move", Kind: "mutation", Asks: true, Input: "id, status?", Description: "Move a plan to another column, " +
					"keeping its place in the column it lands in, and tell every card showing plans so they redraw without a refresh"},
			}},
			{Name: "vercel", Dir: "env", Methods: []plugins.ListedMethod{{Name: "vercel.deploys", Kind: "query", Input: "project?"}}},
		},
		Servers: []mcp.ServerTools{
			{Server: "linear", Label: "Linear", Tools: []mcp.ToolInfo{
				{Name: "get_issue", ReadOnly: true, Description: "Get one issue.\nWith its comments.", Input: schema(`{"properties":{"id":{},"comments":{}},"required":["id"]}`)},
				{Name: "save_issue", AsksFirst: true, Description: "Create or update an issue"},
			}},
			{Server: "vercel", Label: "Vercel", Tools: vercelTools},
			{Server: "stripe", Label: "Stripe", Tools: []mcp.ToolInfo{}, Error: "401 Unauthorized"},
		},
		HostTools: []rpc.ToolDef{
			{Method: "actions.run", Description: "Run an action to the end and answer with its output.", InputSchema: map[string]any{
				"properties": map[string]any{"name": map[string]any{}, "path": map[string]any{}},
			}},
			{Method: "git.status", Description: "A project's branch and changes.", InputSchema: map[string]any{}},
		},
	}
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

// routerModels is a router agent's long list, past what an agent line holds.
func routerModels(n int) []agentcatalog.Model {
	out := make([]agentcatalog.Model, n)
	for i := range out {
		out[i] = agentcatalog.Model{ID: fmt.Sprintf("router/m%02d", i)}
	}
	return out
}
