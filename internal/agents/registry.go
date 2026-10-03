// Package agents constructs provider instances and wires their optional capabilities.
package agents

import (
	"errors"
	"fmt"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agentcatalog"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/agentusage"
	"github.com/repogo/host/internal/chatwire"
	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/mcp"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/shipping"
)

// Factory builds one provider for one host. It returns fresh state every call:
// two hosts in one process, as in tests, never share an adapter or a cache.
type Factory func(agent.Dependencies) agent.Identity

// factories is every production provider, in report order.
var factories = []Factory{
	func(d agent.Dependencies) agent.Identity { return claude.New(d) },
	func(d agent.Dependencies) agent.Identity { return codex.New(d) },
}

// Registry is one host's providers, sorted by what each can do. Every slice is
// in registry order; a provider appears only in the slices it implements.
type Registry struct {
	Agents   []agent.Identity
	Adapters []agent.Adapter
	Catalogs []agentcatalog.Provider
	Usage    []agentusage.Provider
	Sessions []session.Provider
	Hooks    []notify.Provider
	MCP      []mcp.Provider
	Shipping []shipping.Provider
	Labelers []chatwire.Labeler
	// Providers is each agent whose CLI the host installs and signs in.
	Providers agent.Providers
}

// New constructs the production providers for one host.
func New(deps agent.Dependencies) (*Registry, error) { return Construct(deps, factories...) }

// Construct builds a registry from any factories, so tests can add providers
// that run no CLI; only identity is mandatory. A kind may appear once.
func Construct(deps agent.Dependencies, factories ...Factory) (*Registry, error) {
	r := &Registry{}
	seen := map[agent.Kind]bool{}
	for _, makeAgent := range factories {
		p := makeAgent(deps)
		if p == nil || p.Kind() == "" || p.Name() == "" {
			r.Close()
			return nil, errors.New("agent requires kind and name")
		}
		if seen[p.Kind()] {
			r.Close()
			return nil, fmt.Errorf("duplicate agent %q", p.Kind())
		}
		seen[p.Kind()] = true
		r.Agents = append(r.Agents, p)
		caps := []string{}
		if c, ok := p.(agent.Adapter); ok {
			r.Adapters = append(r.Adapters, c)
			caps = append(caps, agent.CapabilityRun)
		}
		if c, ok := p.(agentcatalog.Provider); ok {
			r.Catalogs = append(r.Catalogs, c)
			caps = append(caps, agent.CapabilityModels)
		}
		if c, ok := p.(agentusage.Provider); ok {
			r.Usage = append(r.Usage, c)
			caps = append(caps, agent.CapabilityUsage)
		}
		if _, ok := p.(agentusage.Resetter); ok {
			caps = append(caps, agent.CapabilityUsageReset)
		}
		if c, ok := p.(session.Source); ok {
			r.Sessions = append(r.Sessions, c.Sessions())
			caps = append(caps, agent.CapabilityHistory)
		}
		if c, ok := p.(notify.HookSource); ok {
			r.Hooks = append(r.Hooks, c.Hooks())
			caps = append(caps, agent.CapabilityHooks)
		}
		if c, ok := p.(mcp.Provider); ok {
			r.MCP = append(r.MCP, c)
			caps = append(caps, agent.CapabilityMCP)
		}
		if c, ok := p.(chatwire.Labeler); ok {
			r.Labelers = append(r.Labelers, c)
		}
		if c, ok := p.(shipping.Provider); ok {
			r.Shipping = append(r.Shipping, c)
			caps = append(caps, agent.CapabilityShipping)
		}
		if c, ok := p.(agent.InstallProvider); ok {
			d := c.Definition()
			d.Tool.Capabilities = caps
			if d.Tool.Spec.Command != "" {
				d.Tool.Capabilities = append(d.Tool.Capabilities, clitool.CapabilityInstall, clitool.CapabilityUpdate)
			}
			if d.Tool.Spec.LoginRead != nil {
				d.Tool.Capabilities = append(d.Tool.Capabilities, clitool.CapabilityLogin)
			}
			r.Providers = append(r.Providers, d)
		}
	}
	return r, nil
}

// Tools is every agent's CLI, for the host's one tool inventory.
func (r *Registry) Tools() []clitool.Tool {
	out := make([]clitool.Tool, 0, len(r.Providers))
	for _, p := range r.Providers {
		out = append(out, p.Tool)
	}
	return out
}

// Kinds is every agent this host knows, in registry order.
func (r *Registry) Kinds() []agent.Kind {
	out := make([]agent.Kind, 0, len(r.Agents))
	for _, p := range r.Agents {
		out = append(out, p.Kind())
	}
	return out
}

// Close releases every provider that holds resources, such as a running agent process.
func (r *Registry) Close() {
	for _, p := range r.Agents {
		if c, ok := p.(agent.Closer); ok {
			c.Close()
		}
	}
}

// Name is kind's label. A kind this host does not know, such as a chat from
// an agent it no longer registers, still needs one, so it gets "Agent".
func (r *Registry) Name(kind agent.Kind) string {
	if p, ok := agent.Find(r.Agents, kind, agent.Identity.Kind); ok {
		return p.Name()
	}
	return "Agent"
}
