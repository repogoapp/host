package hostsetup

import (
	"context"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agentcatalog"
	"github.com/repogo/host/internal/clitool"
	ghcore "github.com/repogo/host/internal/github"
	"github.com/repogo/host/internal/hostinfo"
)

type fakeHost struct{ release hostinfo.Release }

func (f fakeHost) Release(context.Context) hostinfo.Release { return f.release }

type fakeGitHub struct{ folder ghcore.CloneFolder }

func (f fakeGitHub) CloneFolder() ghcore.CloneFolder { return f.folder }

type fakeTools struct{ installs []clitool.Install }

func (f fakeTools) List(context.Context) []clitool.Install { return f.installs }

type fakeRunner struct{ agents []agent.Agent }

func (f fakeRunner) Agents() []agent.Agent { return f.agents }

// fakeCatalog answers every kind and records what it was asked.
type fakeCatalog struct{ asked chan agent.Kind }

func (f fakeCatalog) One(_ context.Context, kind agent.Kind, refresh bool) agentcatalog.Catalog {
	f.asked <- kind
	return agentcatalog.Catalog{Agent: string(kind), Available: true, Detail: map[bool]string{true: "refreshed"}[refresh]}
}

// agentTool is an agent's install as the inventory reports it.
func agentTool(kind agent.Kind, installed, authed bool) clitool.Install {
	return clitool.Install{Kind: string(kind), Category: clitool.CategoryAgent, Installed: installed, Authed: authed}
}

func sources(tools []clitool.Install, runs []agent.Agent) (Sources, chan agent.Kind) {
	asked := make(chan agent.Kind, 8)
	return Sources{
		Host:    fakeHost{hostinfo.Release{Version: "1.2.0", Latest: "1.3.0", Status: "behind", CanUpdate: true}},
		GitHub:  fakeGitHub{ghcore.CloneFolder{Dir: "/clones"}},
		Tools:   fakeTools{tools},
		Runner:  fakeRunner{runs},
		Catalog: fakeCatalog{asked},
	}, asked
}

func TestSnapshotComposesTheHostToolsAndAgents(t *testing.T) {
	src, _ := sources(
		[]clitool.Install{
			{Kind: "github", Category: clitool.CategorySourceControl, Installed: true, Authed: true},
			agentTool(agent.KindClaude, true, true),
			{Kind: "vercel", Category: clitool.CategoryCloud, Installed: true},
		},
		[]agent.Agent{{Kind: agent.KindClaude, Available: true}})
	got := Snapshot(t.Context(), src, false)
	if got.Host.Status != "behind" || got.GitHub.Dir != "/clones" || len(got.Tools) != 3 || len(got.Agents) != 1 {
		t.Fatalf("Snapshot = %+v", got)
	}
	claude := got.Agents[0]
	if claude.Kind != agent.KindClaude || !claude.Available || claude.Reason != "" || !claude.Catalog.Available {
		t.Fatalf("claude = %+v", claude)
	}
}

func TestSnapshotSaysWhyAnAgentCannotRun(t *testing.T) {
	const other agent.Kind = "other"
	src, asked := sources(
		[]clitool.Install{
			agentTool(agent.KindClaude, true, false),
			agentTool(agent.KindCodex, false, false),
			agentTool(other, true, true),
		},
		[]agent.Agent{{Kind: agent.KindClaude, Available: true}, {Kind: agent.KindCodex, Available: true}})
	got := Snapshot(t.Context(), src, false)
	want := map[agent.Kind]string{
		agent.KindClaude: "not signed in",
		agent.KindCodex:  "not installed",
		other:            "not supported on this host",
	}
	for _, a := range got.Agents {
		if a.Available || a.Reason != want[a.Kind] {
			t.Errorf("%s: available %v, reason %q; want %q", a.Kind, a.Available, a.Reason, want[a.Kind])
		}
	}
	close(asked)
	for kind := range asked {
		if kind == agent.KindCodex {
			t.Error("asked a CLI that is not installed for its catalog")
		}
	}
}

func TestSnapshotRefreshReachesTheCatalog(t *testing.T) {
	src, _ := sources(
		[]clitool.Install{agentTool(agent.KindClaude, true, true)},
		[]agent.Agent{{Kind: agent.KindClaude, Available: true}})
	if got := Snapshot(t.Context(), src, true); got.Agents[0].Catalog.Detail != "refreshed" {
		t.Fatalf("catalog = %+v, want a refreshed read", got.Agents[0].Catalog)
	}
}

func TestSnapshotHasEmptyArrays(t *testing.T) {
	src, _ := sources([]clitool.Install{}, nil)
	got := Snapshot(t.Context(), src, false)
	if got.Agents == nil || len(got.Agents) != 0 || got.Tools == nil {
		t.Fatalf("snapshot = %#v, want empty arrays", got)
	}
}
