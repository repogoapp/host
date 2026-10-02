// Package agentcatalog asks each agent CLI, in its own protocol, what a caller
// can pick: models, the effort each supports, permission and plan modes, fast
// mode, sandboxing. Read from the CLI rather than hardcoded, because the model
// list changes with every release and a stale one puts a model the user cannot
// run in a picker.
package agentcatalog

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/par"
)

// Catalog is everything one agent lets a caller choose, with its current
// defaults. Value strings are the CLI's own (`opus[1m]`, `gpt-6-astra`,
// `xhigh`) so they can be handed straight back as turn config.
type Catalog struct {
	// How the picker labels the agent: its name, asset name, and the company
	// whose models it runs; stamped by the Service.
	Name      string `json:"name"`
	Icon      string `json:"icon"`
	Company   string `json:"company"`
	Agent     string `json:"agent"`
	Available bool   `json:"available"`

	// Where the answer came from, or why there is none.
	Detail       string `json:"detail,omitempty"`
	CapturedAtMS int64  `json:"captured_at_ms,omitempty"`

	Models []Model `json:"models" wire:"array"`

	// The model in force when none is chosen, and the effort in force for it.
	DefaultModel  string `json:"default_model,omitempty"`
	DefaultEffort string `json:"default_effort,omitempty"`

	// Agent vs. plan: both CLIs have a mode that reads and proposes without
	// editing.
	Modes []Choice `json:"modes" wire:"array"`

	// How freely the agent may act. Claude's permission modes; Codex's
	// approval policies. Empty value means the CLI's default.
	PermissionModes   []Choice `json:"permission_modes" wire:"array"`
	DefaultPermission string   `json:"default_permission,omitempty"`

	// Codex only: what the sandbox lets a command touch.
	Sandboxes      []Choice `json:"sandboxes,omitempty"`
	DefaultSandbox string   `json:"default_sandbox,omitempty"`

	// Codex only: who answers approval prompts.
	Reviewers []Choice `json:"reviewers,omitempty"`

	// Codex only: response tone.
	Personalities      []Choice `json:"personalities,omitempty"`
	DefaultPersonality string   `json:"default_personality,omitempty"`

	// Whether the agent can go faster for more quota, and whether that is on
	// by default. Per model too, since only some models offer it.
	FastMode        bool `json:"fast_mode"`
	FastModeDefault bool `json:"fast_mode_default"`
}

// Model is one entry in the CLI's own picker.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// What a phone shows: Name, or a name read off the id; see label.
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`

	// What the alias resolves to, when the CLI says; `opus[1m]` is
	// `claude-opus-5[1m]`.
	Resolved string `json:"resolved,omitempty"`

	Default bool `json:"default"`

	// Listed by the CLI but not offered in its own picker.
	Hidden bool `json:"hidden,omitempty"`

	// Effort levels this model accepts, in the CLI's order. Empty means the
	// model takes none and an effort control should be disabled.
	Efforts       []Choice `json:"efforts" wire:"array"`
	DefaultEffort string   `json:"default_effort,omitempty"`

	// Fast tiers this model offers; empty means none.
	SpeedTiers []Choice `json:"speed_tiers,omitempty"`

	// Claude: the model decides how much to think per request.
	AdaptiveThinking bool `json:"adaptive_thinking,omitempty"`

	// Claude: the model may run in `auto` permission mode.
	AutoMode bool `json:"auto_mode,omitempty"`

	// text, image, ...; absent when the CLI does not say.
	Modalities []string `json:"modalities,omitempty"`
}

// Choice is one pickable value with the CLI's label for it.
type Choice struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Provider is an agent that can list what a caller may pick, read from the
// CLI in its own protocol. A failed read returns Available false with Detail
// saying why, never an error.
type Provider interface {
	Kind() agent.Kind
	Catalog(context.Context) Catalog
}

// Service caches what it reads. A catalog changes when a CLI is upgraded and
// costs a process spawn to read, so it is kept for hours; a failed read is not
// kept at all, so an agent installed mid-session is asked again next time.
type Service struct {
	// Every agent this host knows, so one without a catalog reads as
	// unsupported rather than unknown.
	kinds     []agent.Kind
	providers []Provider
	mu        sync.Mutex
	fresh     map[agent.Kind]cached
}

type cached struct {
	catalog Catalog
	at      time.Time
}

const (
	ttl = 6 * time.Hour

	// Both CLIs answer in a few seconds; one that hangs reports unavailable
	// rather than holding the sheet.
	readTimeout = 25 * time.Second
)

// New reads catalogs from providers; kinds is every agent the host knows.
func New(kinds []agent.Kind, providers ...Provider) *Service {
	return &Service{kinds: kinds, providers: providers, fresh: map[agent.Kind]cached{}}
}

// All reads every agent concurrently; the slow one does not hold up the sheet.
// refresh drops what was cached first, for after an update.
func (s *Service) All(ctx context.Context, refresh bool) []Catalog {
	return par.Map(s.providers, 0, func(p Provider) Catalog { return s.One(ctx, p.Kind(), refresh) })
}

// Warm reads kinds' catalogs into the cache, for a host starting up: the
// first picker a phone opens is then answered without a CLI spawn.
func (s *Service) Warm(ctx context.Context, kinds []agent.Kind) {
	par.Map(kinds, 0, func(k agent.Kind) Catalog { return s.One(ctx, k, false) })
}

// One reads one agent's catalog, from cache when it has not aged out and
// refresh is false.
func (s *Service) One(ctx context.Context, kind agent.Kind, refresh bool) Catalog {
	if refresh {
		s.forget(kind)
	} else if c, ok := s.cachedValue(kind); ok {
		return c
	}

	provider, ok := agent.Find(s.providers, kind, Provider.Kind)
	if !ok {
		detail := "unknown agent"
		if slices.Contains(s.kinds, kind) {
			detail = "model discovery is not supported"
		}
		c := Unavailable(detail)
		c.Agent = string(kind)
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	catalog := identify(provider, provider.Catalog(ctx))

	if catalog.Available {
		s.mu.Lock()
		s.fresh[kind] = cached{catalog: catalog, at: time.Now()}
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
		c.Name, c.Icon, c.Company = def.Tool.Name, def.Tool.Icon, def.Company
	}
	namer, _ := p.(Namer)
	for i := range c.Models {
		c.Models[i].Label = label(namer, c.Models[i])
	}
	return c
}

func (s *Service) forget(kind agent.Kind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.fresh, kind)
}

func (s *Service) cachedValue(kind agent.Kind) (Catalog, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.fresh[kind]
	if !ok || time.Since(c.at) > ttl {
		return Catalog{}, false
	}
	return c.catalog, true
}

// Unavailable is a catalog with nothing to pick and detail saying why; the
// lists are empty rather than null so a client can range over them.
func Unavailable(detail string) Catalog {
	return Catalog{
		Detail: detail,
		Models: []Model{}, Modes: []Choice{}, PermissionModes: []Choice{},
	}
}
