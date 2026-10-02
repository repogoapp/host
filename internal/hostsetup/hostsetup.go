// Package hostsetup is whether this machine is ready to work from a phone: its
// own release and every CLI it installs and signs in (gh, the agents, the
// cloud CLIs), read together so a screen asks once.
package hostsetup

import (
	"context"
	"sync"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agentcatalog"
	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/emit"
	ghcore "github.com/repogo/host/internal/github"
	"github.com/repogo/host/internal/hostinfo"
	"github.com/repogo/host/internal/par"
)

func init() {
	emit.Register(Setup{})
}

// Setup is the host.setup reply, and the host.setup_changed push when a
// sign-in ends or an install or update lands, so every device replaces its copy whole.
type Setup struct {
	Host hostinfo.Release `json:"host"`

	// Tools is every CLI the host installs and signs in: gh, the agents and
	// the cloud CLIs, each with the category its Environment section shows.
	Tools []clitool.Install `json:"tools" wire:"array"`

	// Agents is what only an agent has: whether a turn can start with it now
	// and what its CLI offers to pick. Its install is in Tools, by kind.
	Agents []Agent `json:"agents" wire:"array"`

	GitHub ghcore.CloneFolder `json:"github"`
}

func (Setup) Method() string { return "host.setup_changed" }

// Agent is one agent: whether a turn can start with it now (Reason says what
// is missing), and what its CLI offers to pick.
type Agent struct {
	Kind      agent.Kind           `json:"kind"`
	Available bool                 `json:"available"`
	Reason    string               `json:"reason,omitempty"`
	Catalog   agentcatalog.Catalog `json:"catalog"`
}

// Sources is what a snapshot reads: this host's release, where GitHub clones
// land, every CLI, the adapters that run the agents, and their catalogs.
type Sources struct {
	Host interface {
		Release(ctx context.Context) hostinfo.Release
	}
	GitHub interface {
		CloneFolder() ghcore.CloneFolder
	}
	Tools interface {
		List(ctx context.Context) []clitool.Install
	}
	Runner interface {
		Agents() []agent.Agent
	}
	Catalog interface {
		One(ctx context.Context, kind agent.Kind, refresh bool) agentcatalog.Catalog
	}
}

// Snapshot reads the release and every CLI concurrently, since each is
// subprocesses or a network round trip. refresh re-reads the catalogs, for
// after a sign-in, install or update changed what a CLI offers.
func Snapshot(ctx context.Context, src Sources, refresh bool) Setup {
	out := Setup{GitHub: src.GitHub.CloneFolder()}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); out.Host = src.Host.Release(ctx) }()
	go func() {
		defer wg.Done()
		out.Tools = src.Tools.List(ctx)
		out.Agents = agents(ctx, src, out.Tools, refresh)
	}()
	wg.Wait()
	return out
}

func agents(ctx context.Context, src Sources, tools []clitool.Install, refresh bool) []Agent {
	adapters := map[agent.Kind]agent.Agent{}
	for _, a := range src.Runner.Agents() {
		adapters[a.Kind] = a
	}
	installs := []clitool.Install{}
	for _, tool := range tools {
		if tool.Category == clitool.CategoryAgent {
			installs = append(installs, tool)
		}
	}
	return par.Map(installs, 0, func(install clitool.Install) Agent {
		kind := agent.Kind(install.Kind)
		out := Agent{Kind: kind}
		adapter, runs := adapters[kind]
		switch {
		case !runs:
			out.Reason = "not supported on this host"
		case !adapter.Available:
			out.Reason = adapter.Reason
		case !install.Installed:
			out.Reason = "not installed"
		case !install.Authed:
			out.Reason = "not signed in"
		default:
			out.Available = true
		}
		// A missing CLI has nothing to list, and asking would spawn nothing.
		if install.Installed {
			out.Catalog = src.Catalog.One(ctx, kind, refresh)
		} else {
			out.Catalog = agentcatalog.Unavailable("not installed")
			out.Catalog.Agent, out.Catalog.Name, out.Catalog.Icon = install.Kind, install.Name, install.Icon
		}
		return out
	})
}
