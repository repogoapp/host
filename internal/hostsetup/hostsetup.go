// Package hostsetup is whether this machine is ready to work from a phone: its
// own release and every CLI it installs and signs in (gh, the agents, the
// cloud CLIs), read together so a screen asks once.
package hostsetup

import (
	"cmp"
	"context"
	"sync"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentcatalog"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/emit"
	ghcore "github.com/repogoapp/host/internal/github"
	"github.com/repogoapp/host/internal/hostinfo"
	"github.com/repogoapp/host/internal/par"
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

	// Agents is what each agent's CLI offers to pick. Its install is in
	// Tools, by kind.
	Agents []agentcatalog.Catalog `json:"agents" wire:"array"`

	GitHub ghcore.CloneFolder `json:"github"`
}

func (Setup) Method() string { return "host.setup_changed" }

// Sources is what a snapshot reads: this host's release, where GitHub clones
// land, every CLI, and the agents' catalogs.
type Sources struct {
	Host interface {
		Release(ctx context.Context) hostinfo.Release
	}
	GitHub interface {
		CloneFolder() ghcore.CloneFolder
	}
	Tools interface {
		List(ctx context.Context) []clitool.Install
		AccountKey(kind string) string
	}
	Catalog interface {
		One(ctx context.Context, kind agent.Kind, install agentcatalog.Install, refresh bool) agentcatalog.Catalog
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

func agents(ctx context.Context, src Sources, tools []clitool.Install, refresh bool) []agentcatalog.Catalog {
	installs := []clitool.Install{}
	for _, tool := range tools {
		if tool.Category == clitool.CategoryAgent {
			installs = append(installs, tool)
		}
	}
	return par.Map(installs, 0, func(install clitool.Install) agentcatalog.Catalog {
		// A missing CLI has nothing to list, and asking would spawn nothing.
		if !install.Installed {
			out := agentcatalog.Unavailable()
			out.Agent, out.Name = install.Kind, install.Name
			return out
		}
		return src.Catalog.One(ctx, agent.Kind(install.Kind), catalogInstall(install, src.Tools.AccountKey(install.Kind)), refresh)
	})
}

// catalogInstall is what an agent's model list is read from: the CLI's version
// and its account, either of which changes what it offers. key is the
// account's seat, which names it best.
func catalogInstall(install clitool.Install, key string) agentcatalog.Install {
	out := agentcatalog.Install{Version: install.Version}
	if install.Account != nil {
		out.Account = cmp.Or(key, install.Account.Email)
	}
	return out
}
