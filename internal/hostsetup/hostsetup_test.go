package hostsetup

import (
	"context"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentcatalog"
	"github.com/repogoapp/host/internal/clitool"
	ghcore "github.com/repogoapp/host/internal/github"
	"github.com/repogoapp/host/internal/hostinfo"
)

type fakeHost struct{ release hostinfo.Release }

func (f fakeHost) Release(context.Context) hostinfo.Release { return f.release }

type fakeGitHub struct{ folder ghcore.CloneFolder }

func (f fakeGitHub) CloneFolder() ghcore.CloneFolder { return f.folder }

type fakeTools struct{ installs []clitool.Install }

func (f fakeTools) List(context.Context) []clitool.Install { return f.installs }

// fakeCatalog answers every kind and records what it was asked.
type fakeCatalog struct{ asked chan agent.Kind }

func (f fakeCatalog) One(_ context.Context, kind agent.Kind, _ agentcatalog.Install, refresh bool) agentcatalog.Catalog {
	f.asked <- kind
	return agentcatalog.Catalog{Agent: string(kind), Available: true, DefaultModel: map[bool]string{true: "refreshed"}[refresh]}
}

// agentTool is an agent's install as the inventory reports it.
func agentTool(kind agent.Kind, installed, authed bool) clitool.Install {
	return clitool.Install{Kind: string(kind), Category: clitool.CategoryAgent, Installed: installed, Authed: authed}
}

func sources(tools []clitool.Install) (Sources, chan agent.Kind) {
	asked := make(chan agent.Kind, 8)
	return Sources{
		Host:    fakeHost{hostinfo.Release{Version: "1.2.0", Latest: "1.3.0", Status: "behind", CanUpdate: true}},
		GitHub:  fakeGitHub{ghcore.CloneFolder{Dir: "/clones"}},
		Tools:   fakeTools{tools},
		Catalog: fakeCatalog{asked},
	}, asked
}

func TestSnapshotComposesTheHostToolsAndAgents(t *testing.T) {
	src, _ := sources([]clitool.Install{
		{Kind: "github", Category: clitool.CategorySourceControl, Installed: true, Authed: true},
		agentTool(agent.KindClaude, true, true),
		{Kind: "vercel", Category: clitool.CategoryCloud, Installed: true},
	})
	got := Snapshot(t.Context(), src, false)
	if got.Host.Status != "behind" || got.GitHub.Dir != "/clones" || len(got.Tools) != 3 || len(got.Agents) != 1 {
		t.Fatalf("Snapshot = %+v", got)
	}
	if claude := got.Agents[0]; claude.Agent != string(agent.KindClaude) || !claude.Available {
		t.Fatalf("claude = %+v", claude)
	}
}

func TestSnapshotSkipsTheCatalogOfAMissingCLI(t *testing.T) {
	src, asked := sources([]clitool.Install{
		agentTool(agent.KindClaude, true, false),
		agentTool(agent.KindCodex, false, false),
	})
	got := Snapshot(t.Context(), src, false)
	for _, a := range got.Agents {
		if a.Agent == string(agent.KindCodex) && (a.Available || a.Models == nil) {
			t.Errorf("codex = %+v, want unavailable with an empty list", a)
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
	src, _ := sources([]clitool.Install{agentTool(agent.KindClaude, true, true)})
	if got := Snapshot(t.Context(), src, true); got.Agents[0].DefaultModel != "refreshed" {
		t.Fatalf("catalog = %+v, want a refreshed read", got.Agents[0])
	}
}

func TestSnapshotHasEmptyArrays(t *testing.T) {
	src, _ := sources([]clitool.Install{})
	got := Snapshot(t.Context(), src, false)
	if got.Agents == nil || len(got.Agents) != 0 || got.Tools == nil {
		t.Fatalf("snapshot = %#v, want empty arrays", got)
	}
}
