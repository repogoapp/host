// Package catalog is the `catalog.*` methods: what an environment or one of its
// projects offers to call, each part read at once and rendered by internal/catalog.
package catalog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	catalogcore "github.com/repogoapp/host/internal/catalog"
	"github.com/repogoapp/host/internal/files"
	"github.com/repogoapp/host/internal/hostsetup"
	mcpcore "github.com/repogoapp/host/internal/mcp"
	pluginscore "github.com/repogoapp/host/internal/plugins"
	"github.com/repogoapp/host/internal/projectagent"
	"github.com/repogoapp/host/internal/rpc"
)

// Wait is how long a read waits on its parts: a slow MCP server or a plugin
// runtime still starting shows as missing rather than holding the read.
const Wait = 3 * time.Second

type Deps struct {
	Files *files.Service
	// Home is where an environment's read lists from: its own plugins and the
	// MCP servers on everywhere.
	Home          string
	Setup         hostsetup.Sources
	ProjectAgents projectagent.Lister
	Plugins       interface {
		Plugins(ctx context.Context, path string) ([]pluginscore.Listed, error)
	}
	MCP interface {
		Tools(ctx context.Context, cwd string, described bool) []mcpcore.ServerTools
	}
	Wait time.Duration

	hostTools func() []rpc.ToolDef
}

type ReadParams struct {
	Project string `json:"project" doc:"The project's folder; empty is the environment alone"`
	Name    string `json:"name" doc:"One plugin (plans) or MCP server (mcp:linear) to answer in full"`
}

// ReadResult is the catalog as text, the way a voice session reads it.
type ReadResult struct {
	Text string `json:"text"`
}

func Register(r *rpc.Router, d Deps) {
	// Every family has registered by the first read.
	d.hostTools = sync.OnceValue(r.ToolDefs)
	rpc.Add(r, "catalog.read", d.read)
}

// read is the environment's agents, plugins and MCP servers, or with a project
// its own agents and the host's tools too; with a name, that one in full.
func (d Deps) read(ctx context.Context, _ rpc.Caller, p ReadParams) (ReadResult, error) {
	path := d.Home
	if p.Project != "" {
		contained, err := d.Files.Contain(p.Project)
		if err != nil {
			return ReadResult{}, err
		}
		path = contained
	}
	whole := p.Name == ""
	server := strings.HasPrefix(p.Name, "mcp:")
	plugins := startIf(whole || !server, func() ([]pluginscore.Listed, error) { return d.plugins(ctx, path) })
	servers := startIf(whole || server, func() ([]mcpcore.ServerTools, error) { return d.MCP.Tools(ctx, path, true), nil })
	agents := startIf(whole, func() ([]hostsetup.Agent, error) { return hostsetup.Agents(ctx, d.Setup, false), nil })
	projectAgents := startIf(whole && p.Project != "", func() ([]projectagent.ProjectAgent, error) {
		return d.ProjectAgents.ProjectAgents(path)
	})

	wait, cancel := context.WithTimeout(ctx, d.Wait)
	defer cancel()
	var parts catalogcore.Parts
	parts.Plugins, parts.PluginsErr = await(wait, plugins)
	parts.Servers, parts.ServersErr = await(wait, servers)
	if !whole {
		text, err := catalogcore.Detail(parts, p.Name)
		return ReadResult{Text: text}, err
	}
	parts.Agents, parts.AgentsErr = await(wait, agents)
	parts.ProjectAgents, parts.ProjectAgentsErr = await(wait, projectAgents)
	if p.Project != "" {
		parts.HostTools = d.hostTools()
	}
	return ReadResult{Text: catalogcore.Render(parts)}, nil
}

// plugins is every plugin the folder can use; a host without the runtime, or
// a folder with none, has none rather than an error.
func (d Deps) plugins(ctx context.Context, path string) ([]pluginscore.Listed, error) {
	listed, err := d.Plugins.Plugins(ctx, path)
	if errors.Is(err, pluginscore.ErrOff) || errors.Is(err, pluginscore.ErrNoPlugins) {
		return nil, nil
	}
	return listed, err
}

var errLate = errors.New("it didn't answer in time")

// answer is one part's value, or why it has none.
type answer[T any] struct {
	value T
	err   error
}

// start reads one part on its own goroutine, so every part runs at once.
func start[T any](read func() (T, error)) <-chan answer[T] {
	out := make(chan answer[T], 1)
	go func() {
		value, err := read()
		out <- answer[T]{value, err}
	}()
	return out
}

// startIf is start, or a part with nothing in it when the read doesn't need it.
func startIf[T any](need bool, read func() (T, error)) <-chan answer[T] {
	if !need {
		out := make(chan answer[T], 1)
		out <- answer[T]{}
		return out
	}
	return start(read)
}

// await is a part's answer, or errLate when wait ends first; the late part's
// answer is dropped. One that landed as the wait ended still counts: select
// picks at random when both are ready.
func await[T any](wait context.Context, part <-chan answer[T]) (T, error) {
	select {
	case a := <-part:
		return a.value, a.err
	case <-wait.Done():
		select {
		case a := <-part:
			return a.value, a.err
		default:
			var zero T
			return zero, errLate
		}
	}
}
