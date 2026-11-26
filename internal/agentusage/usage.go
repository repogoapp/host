// Package agentusage reports how much of each provider's rate limit this
// machine has spent, from state the CLI already has. The sources are
// undocumented, so both degrade to "unavailable" rather than erroring.
package agentusage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/par"
)

// Usage is one provider's rate-limit picture.
type Usage struct {
	// The agent's name, for the row label; Name and Agent are stamped by the Service.
	Name      string `json:"name,omitempty"`
	Agent     string `json:"agent"`
	Available bool   `json:"available"`

	Plan string `json:"plan"`

	// Who these windows belong to; a last-known reading keeps the account it
	// was read for, so an account switch never relabels old numbers.
	Account *Account `json:"account,omitempty"`

	// When the reading was taken, NOT when it was served. A cached or
	// last-known-good answer keeps its original time so the client can show its
	// age rather than claiming to be current.
	CapturedAtMS int64 `json:"captured_at_ms"`

	Windows []Window `json:"windows" wire:"array"`

	// Resets are grants that wipe the current windows early when spent (see
	// Service.Reset). Only Codex has them, and only its account endpoint lists
	// them, so a reading from the logs carries none.
	Resets *Resets `json:"resets,omitempty"`

	// What is left to spend on an account billed by amount, not by window;
	// absent when the provider reports none.
	Balance *Balance `json:"balance,omitempty"`
}

// Account is who a reading belongs to, present while the CLI is signed in:
// its email, masked where it is read (clitool.MaskEmail). It carries no id,
// so the phone shows each environment's limits on their own.
type Account struct {
	Email string `json:"email,omitempty"`
}

// AccountOf is the CLI's sign-in as a reading's account; nil when signed out.
func AccountOf(a *clitool.Account) *Account {
	if a == nil {
		return nil
	}
	return &Account{Email: a.Email}
}

// Balance is prepaid credit left on the account.
type Balance struct {
	Remaining float64 `json:"remaining"`
	// "usd" or "credits"; the client formats the amount by it.
	Unit string `json:"unit"`
}

// Resets is the account's reset credits as Codex reports them.
type Resets struct {
	// Can exceed len(Credits): the provider may list fewer than it counts.
	Available int64         `json:"available"`
	Credits   []ResetCredit `json:"credits" wire:"array"`
}

type ResetCredit struct {
	ID    string `json:"id"`
	Title string `json:"title"`

	// "available", "redeeming", "redeemed", or "unknown".
	Status string `json:"status"`

	// Unix seconds; zero when the credit does not expire.
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

// Service caches what it reads for a minute, so a popover opened on every
// keystroke doesn't start a CLI each time. Past the minute it answers with the
// old reading at once and reads again behind it, so a screen never waits on a
// CLI except for an agent's first reading.
type Service struct {
	providers []Provider
	mu        sync.Mutex
	fresh     map[agent.Kind]cached
	lastGood  map[agent.Kind]Usage
	// refreshing is the agents being read behind an old answer, one read each.
	refreshing map[agent.Kind]bool
	// generation counts each agent's invalidations (Reset), so a read begun
	// before one never stores its older answer after it.
	generation map[agent.Kind]int
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

// New reads usage from providers.
func New(providers ...Provider) *Service {
	return &Service{
		providers:  providers,
		fresh:      map[agent.Kind]cached{},
		lastGood:   map[agent.Kind]Usage{},
		refreshing: map[agent.Kind]bool{},
		generation: map[agent.Kind]int{},
	}
}

// All reports every provider concurrently, so a slow one does not hold up the
// popover.
func (s *Service) All(ctx context.Context) []Usage {
	return par.Map(s.providers, 0, func(p Provider) Usage { return s.One(ctx, p.Kind()) })
}

// One reports a single provider: a fresh reading as it is, an old one at once
// while it is read again behind, and none only after reading it.
func (s *Service) One(ctx context.Context, kind agent.Kind) Usage {
	s.mu.Lock()
	hit, ok := s.fresh[kind]
	if ok && time.Since(hit.at) <= ttl {
		s.mu.Unlock()
		return hit.usage
	}
	if ok {
		if !s.refreshing[kind] {
			s.refreshing[kind] = true
			go s.read(context.WithoutCancel(ctx), kind)
		}
		s.mu.Unlock()
		return hit.usage
	}
	s.mu.Unlock()
	return s.read(ctx, kind)
}

// read asks the provider and keeps its answer.
func (s *Service) read(ctx context.Context, kind agent.Kind) Usage {
	s.mu.Lock()
	generation := s.generation[kind]
	last := s.lastGood[kind]
	s.mu.Unlock()

	usage := Usage{Agent: string(kind)}
	if provider, ok := agent.Find(s.providers, kind, Provider.Kind); ok {
		ctx, cancel := context.WithTimeout(ctx, readTimeout)
		usage = provider.Usage(ctx, last)
		cancel()
		usage.Agent = string(kind)
		if p, ok := provider.(agent.Identity); ok {
			usage.Name = p.Name()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refreshing, kind)
	if s.generation[kind] != generation {
		return usage
	}
	s.fresh[kind] = cached{usage: usage, at: time.Now()}
	if usage.Available && len(usage.Windows) > 0 {
		s.lastGood[kind] = usage
	}
	return usage
}
