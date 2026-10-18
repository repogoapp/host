// Package agentcatalog asks each agent CLI, in its own protocol, what a caller
// can pick: models, the effort each supports and whether it has a fast mode.
// Read from the CLI rather than hardcoded, because the model list changes with
// every release and a stale one puts a model the user cannot run in a picker.
package agentcatalog

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/par"
)

// Catalog is what one agent lets a caller choose, with its default model.
// Value strings are the CLI's own (`opus[1m]`, `gpt-6-astra`, `xhigh`) so they
// can be handed straight back as turn config.
type Catalog struct {
	// The agent, its name and the company whose models it runs; stamped by
	// the Service.
	Agent     string `json:"agent"`
	Name      string `json:"name"`
	Company   string `json:"company"`
	Available bool   `json:"available"`

	Models []Model `json:"models" wire:"array"`

	// The model in force when none is chosen.
	DefaultModel string `json:"default_model,omitempty"`
}

// Model is one entry in the CLI's own picker.
type Model struct {
	ID string `json:"id"`
	// What a phone shows: read off the id when the agent knows its shape, else
	// the CLI's name; see label.
	Name string `json:"name"`

	// What the alias resolves to, when the CLI says; `opus[1m]` is
	// `claude-opus-5[1m]`.
	Resolved string `json:"resolved,omitempty"`

	Default bool `json:"default"`

	// Listed by the CLI but not offered in its own picker.
	Hidden bool `json:"hidden,omitempty"`

	// Effort levels this model accepts, in the CLI's order. Empty means the
	// model takes none and an effort control should be disabled.
	Efforts       []string `json:"efforts" wire:"array"`
	DefaultEffort string   `json:"default_effort,omitempty"`

	// Whether the model can go faster for more quota.
	FastMode bool `json:"fast_mode,omitempty"`

	// Claude: the model decides how much to think per request.
	AdaptiveThinking bool `json:"adaptive_thinking,omitempty"`

	// The company that makes the model, when it isn't the catalog's own: a
	// router's models name their maker, which a phone asks consent for.
	Company string `json:"company,omitempty"`
}

// Provider is an agent that can list what a caller may pick, read from the
// CLI in its own protocol. An error says why there is no list, for the log.
type Provider interface {
	Kind() agent.Kind
	Catalog(context.Context) (Catalog, error)
}

// Service caches what it reads, while the CLI is the install it was read from:
// an update or another account changes the list. A provider can add a model
// without a release, so a list is also read again after an hour.
type Service struct {
	log       *slog.Logger
	providers []Provider
	mu        sync.Mutex
	fresh     map[agent.Kind]cached
}

// Install is the CLI a catalog was read from: its version and who it is
// signed in as.
type Install struct {
	Version string
	Account string
}

type cached struct {
	catalog Catalog
	install Install
	at      time.Time
}

const (
	ttl = time.Hour

	// Both CLIs answer in a few seconds; one that hangs reports unavailable
	// rather than holding the sheet.
	readTimeout = 25 * time.Second
)

// New reads catalogs from providers, logging why one has none.
func New(log *slog.Logger, providers ...Provider) *Service {
	return &Service{log: log, providers: providers, fresh: map[agent.Kind]cached{}}
}

// Warm reads kinds' catalogs into the cache, for a host starting up: the
// first picker a phone opens is then answered without a CLI spawn.
func (s *Service) Warm(ctx context.Context, kinds []agent.Kind) {
	par.Map(kinds, 0, func(k agent.Kind) Catalog { return s.One(ctx, k, Install{}, false) })
}

// One reads one agent's catalog, from cache when it was read from this
// install within the hour and refresh is false.
func (s *Service) One(ctx context.Context, kind agent.Kind, install Install, refresh bool) Catalog {
	if refresh {
		s.forget(kind)
	} else if c, ok := s.cachedFor(kind, install); ok {
		return c
	}

	provider, ok := agent.Find(s.providers, kind, Provider.Kind)
	if !ok {
		c := Unavailable()
		c.Agent = string(kind)
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	read, err := provider.Catalog(ctx)
	if err != nil {
		s.log.Warn("agent catalog unavailable", "agent", kind, "err", err)
		read = Unavailable()
	}
	catalog := identify(provider, read)

	if catalog.Available {
		s.mu.Lock()
		s.fresh[kind] = cached{catalog: catalog, install: install, at: time.Now()}
		s.mu.Unlock()
	}
	return catalog
}

// identify labels a catalog with the agent it came from and names its
// models; the provider fills in only what its CLI said.
func identify(p Provider, c Catalog) Catalog {
	c.Agent = string(p.Kind())
	if d, ok := p.(agent.InstallProvider); ok {
		def := d.Definition()
		c.Name, c.Company = def.Tool.Name, def.Company
	}
	namer, _ := p.(Namer)
	for i := range c.Models {
		c.Models[i].Name = label(namer, c.Models[i])
	}
	return c
}

func (s *Service) forget(kind agent.Kind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.fresh, kind)
}

// cachedFor is the cached catalog when it is still good for install. One
// warmed at start, before any install was read, takes the first one asked with.
func (s *Service) cachedFor(kind agent.Kind, install Install) (Catalog, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.fresh[kind]
	if !ok || time.Since(c.at) > ttl {
		return Catalog{}, false
	}
	if c.install == (Install{}) {
		c.install = install
		s.fresh[kind] = c
	}
	return c.catalog, c.install == install
}

// Last is the last catalog read, however old, never asking the CLI: for naming
// a model or listing agents, where an older list beats waiting on a spawn.
func (s *Service) Last(kind agent.Kind) (Catalog, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.fresh[kind]
	return c.catalog, ok
}

// Unavailable is a catalog with nothing to pick; its list is empty rather
// than null so a client can range over it.
func Unavailable() Catalog {
	return Catalog{Models: []Model{}}
}
