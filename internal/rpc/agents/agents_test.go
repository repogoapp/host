package agents

import (
	"reflect"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentcatalog"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/files"
	"github.com/repogoapp/host/internal/projectagent"
	"github.com/repogoapp/host/internal/rpc"
)

type listed []projectagent.ProjectAgent

func (l listed) ProjectAgents(string) ([]projectagent.ProjectAgent, error) { return l, nil }

type catalogs map[agent.Kind]agentcatalog.Catalog

func (c catalogs) Last(kind agent.Kind) (agentcatalog.Catalog, bool) {
	catalog, ok := c[kind]
	return catalog, ok
}

// The picker gets one list: the environment's agents, then the project's,
// each in the shape chats.start takes. An environment agent's options are its
// cached catalog's; one not read yet lists none.
func TestListIsTheEnvironmentsAgentsThenTheProjects(t *testing.T) {
	dir := t.TempDir()
	d := Deps{
		Files: files.New(files.Config{Roots: files.StaticRoots{dir}}),
		Environment: agent.Providers{
			{Tool: clitool.Tool{Kind: "claude", Name: "Claude"}},
			{Tool: clitool.Tool{Kind: "codex", Name: "Codex"}},
		},
		Project: listed{{ID: "helper", Name: "Helper", Models: []projectagent.ProjectAgentModel{{ID: "s", Name: "Sonnet"}}, Model: "s"}},
		Catalog: catalogs{"claude": {
			Models: []agentcatalog.Model{
				{ID: "opus", Label: "Opus", Efforts: []agentcatalog.Choice{{Value: "high"}, {Value: "max"}}, DefaultEffort: "high"},
				{ID: "old", Label: "Old", Hidden: true},
			},
			DefaultModel:    "opus",
			Modes:           []agentcatalog.Choice{{Value: "plan", Name: "Plan"}},
			PermissionModes: []agentcatalog.Choice{{Value: "acceptEdits", Name: "Accept edits"}},
		}},
	}
	got, err := d.list(t.Context(), rpc.Caller{}, ListParams{Project: dir})
	if err != nil {
		t.Fatal(err)
	}
	want := []AgentChoice{
		{Agent: "claude", Name: "Claude",
			Models: []AgentModel{{ID: "opus", Name: "Opus", Efforts: []string{"high", "max"}, DefaultEffort: "high"}},
			Model:  "opus", Modes: []string{"agent", "plan"},
			PermissionModes: []string{"approval-required", "auto-accept-edits", "full-access"}},
		{Agent: "codex", Name: "Codex"},
		{Agent: string(agent.KindProject), ProjectAgent: "helper", Name: "Helper",
			Models: []AgentModel{{ID: "s", Name: "Sonnet"}}, Model: "s"},
	}
	if !reflect.DeepEqual(got.Agents, want) {
		t.Fatalf("got %+v", got.Agents)
	}
}
