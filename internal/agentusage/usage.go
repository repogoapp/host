// Package agentusage reports how much of each provider's rate limit this
// machine has spent, from state the CLI already has. The sources are
// undocumented, so both degrade to "unavailable" rather than erroring.
package agentusage

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/par"
)

// Usage is one provider's rate-limit picture.
type Usage struct {
	// The agent's name, for the row label; Name and Agent are stamped by the Service.
	Name      string `json:"name,omitempty"`
	Agent     string `json:"agent"`
	Available bool   `json:"available"`

	Plan string `json:"plan"`
	Tier string `json:"tier"`

	// Who these windows belong to; a last-known reading keeps the account it
	// was read for, so an account switch never relabels old numbers.
	Account *clitool.Account `json:"account,omitempty"`

	// Which window the provider says is currently blocking, when it says so at
	// all. Only Codex reports this, and only from the account endpoint.
	RateLimitReached string `json:"rate_limit_reached"`

	// Where these numbers came from, in words: a log scrape and an account API
	// deserve different trust, and the client cannot tell from the numbers.
	Detail string `json:"detail"`

	// When the reading was taken, NOT when it was served. A cached or
	// last-known-good answer keeps its original time so the client can show its
	// age rather than claiming to be current.
	CapturedAtMS int64 `json:"captured_at_ms"`

	Windows []Window `json:"windows" wire:"array"`

	// Resets are grants that wipe the current windows early when spent (see
	// Service.Reset). Only Codex has them, and only its account endpoint lists
	// them, so a reading from the logs carries none.
	Resets *Resets `json:"resets,omitempty"`
}

// Resets is the account's reset credits as Codex reports them.
type Resets struct {
	// Can exceed len(Credits): the provider may list fewer than it counts.
	Available int64         `json:"available"`
	Credits   []ResetCredit `json:"credits" wire:"array"`
}

type ResetCredit struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`

	// "available", "redeeming", "redeemed", or "unknown".
	Status    string `json:"status"`
	ResetType string `json:"reset_type,omitempty"`

	// Unix seconds; ExpiresAt is zero when the credit does not expire.
	GrantedAt int64 `json:"granted_at,omitempty"`
	ExpiresAt int64 `json:"expires_at"`
}

// Window is one rate-limit bucket — a 5-hour session allowance, a weekly cap.
type Window struct {
	ID    string `json:"id"`
	Label string `json:"label"`

	UsedPercent float64 `json:"used_percent"`

	// How long the window runs; this is what labels a window, see WindowLabel.
	WindowMinutes int64 `json:"window_minutes"`

	// Unix seconds. Zero when the provider did not say.
	ResetsAt int64 `json:"resets_at"`

	Severity string `json:"severity"`
	IsActive bool   `json:"is_active"`

	// Which model the window applies to, when it is model-specific. Claude
	// reports per-model weekly caps alongside the account-wide one.
	Scope string `json:"scope"`
}

// WindowLabel names a window by how long it runs, never by which field it
// arrived in: Codex's `primary` has come back with windowDurationMins 10080.
func WindowLabel(minutes int64, fallback string) string {
	switch {
	case minutes <= 0:
		return fallback
	case minutes < 60:
		return fmt.Sprintf("%dm", minutes)
	case minutes < 60*24:
		return fmt.Sprintf("%dh", minutes/60)
	case minutes == 10080:
		return "Weekly"
	case minutes%(60*24) == 0:
		return fmt.Sprintf("%dd", minutes/(60*24))
	default:
		return fallback
	}
}

// PlanWords turns a provider's plan slug into words, "claude_max" → "Claude
// Max", for a plan the provider's own table does not name.
func PlanWords(slug string) string {
	words := strings.FieldsFunc(slug, func(r rune) bool { return r == '_' || r == '-' || r == ' ' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}
	return strings.Join(words, " ")
}

// Provider is an agent that can report its rate-limit usage. last is the
// previous good reading, zero when there is none, for the provider to serve
// when a live read fails; a failure is Available false, never an error.
type Provider interface {
	Kind() agent.Kind
	Usage(ctx context.Context, last Usage) Usage
}

// Resetter is a Provider whose account has reset credits (see Service.Reset).
type Resetter interface {
	Reset(ctx context.Context, creditID string) (outcome string, err error)
}

// Service caches what it reads. The TTL is a safety measure, not an
// optimization: Claude's usage endpoint rate-limits its callers, and a host
// that asked on every keystroke would lock the user out of the number.
type Service struct {
	// Every agent this host knows, so one without usage reads as unsupported
	// rather than unknown.
	kinds     []agent.Kind
	providers []Provider
	mu        sync.Mutex
	fresh     map[agent.Kind]cached
	lastGood  map[agent.Kind]Usage
}

type cached struct {
	usage Usage
	at    time.Time
}

const (
	ttl = 60 * time.Second

	// A provider that overruns reports unavailable rather than holding the popover.
	readTimeout = 12 * time.Second

	// Spending a credit is an account call through a one-shot CLI.
	resetTimeout = 25 * time.Second
)

// New reads usage from providers; kinds is every agent the host knows.
func New(kinds []agent.Kind, providers ...Provider) *Service {
	return &Service{
		kinds:     kinds,
		providers: providers,
		fresh:     map[agent.Kind]cached{},
		lastGood:  map[agent.Kind]Usage{},
	}
}

// All reports every provider concurrently, so a slow one does not hold up the
// popover.
func (s *Service) All(ctx context.Context) []Usage {
	return par.Map(s.providers, 0, func(p Provider) Usage { return s.One(ctx, p.Kind()) })
}

// One reports a single provider, serving a cached reading when it is fresh.
func (s *Service) One(ctx context.Context, kind agent.Kind) Usage {
	if hit, ok := s.cachedValue(kind); ok {
		return hit
	}

	provider, ok := agent.Find(s.providers, kind, Provider.Kind)
	if !ok {
		if slices.Contains(s.kinds, kind) {
			return Usage{Agent: string(kind), Detail: "usage reporting is not supported"}
		}
		return Usage{Agent: string(kind), Detail: "unknown agent"}
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	usage := provider.Usage(ctx, s.lastGoodValue(kind))
	usage.Agent = string(kind)
	if p, ok := provider.(agent.Identity); ok {
		usage.Name = p.Name()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh[kind] = cached{usage: usage, at: time.Now()}
	if usage.Available && len(usage.Windows) > 0 {
		s.lastGood[kind] = usage
	}
	return usage
}

func (s *Service) cachedValue(kind agent.Kind) (Usage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hit, ok := s.fresh[kind]
	if !ok || time.Since(hit.at) > ttl {
		return Usage{}, false
	}
	return hit.usage, true
}

// lastGoodValue is the last good reading, zero when there is none, for a
// provider to serve with its original CapturedAtMS when a live read fails.
func (s *Service) lastGoodValue(kind agent.Kind) Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastGood[kind]
}
