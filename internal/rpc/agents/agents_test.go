package agents

import (
	"reflect"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/files"
	"github.com/repogoapp/host/internal/projectagent"
	"github.com/repogoapp/host/internal/rpc"
)

type listed []projectagent.ProjectAgent

func (l listed) ProjectAgents(string) ([]projectagent.ProjectAgent, error) { return l, nil }

// The picker gets one list: the environment's agents, then the project's,
// each in the shape chats.start takes.
func TestListIsTheEnvironmentsAgentsThenTheProjects(t *testing.T) {
	dir := t.TempDir()
	d := Deps{
		Files: files.New(files.Config{Roots: files.StaticRoots{dir}}),
		Environment: agent.Providers{
			{Tool: clitool.Tool{Kind: "claude", Name: "Claude"}},
			{Tool: clitool.Tool{Kind: "codex", Name: "Codex"}},
		},
		Project: listed{{ID: "helper", Name: "Helper", Models: []projectagent.ProjectAgentModel{{ID: "s", Name: "Sonnet"}}, Model: "s"}},
	}
	got, err := d.list(t.Context(), rpc.Caller{}, ListParams{Project: dir})
	if err != nil {
		t.Fatal(err)
	}
	want := []AgentChoice{
		{Agent: "claude", Name: "Claude"},
		{Agent: "codex", Name: "Codex"},
		{Agent: string(agent.KindProject), ProjectAgent: "helper", Name: "Helper",
			Models: []projectagent.ProjectAgentModel{{ID: "s", Name: "Sonnet"}}, Model: "s"},
	}
	if !reflect.DeepEqual(got.Agents, want) {
		t.Fatalf("got %+v", got.Agents)
	}
}
