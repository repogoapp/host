package agentusage

import (
	"context"
	"fmt"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/errkind"
)

// ErrNoResets is a reset asked of a provider that has no reset credits.
var ErrNoResets = errkind.New(errkind.Invalid, "this agent has no rate-limit resets")

// Reset spends one of kind's reset credits; an empty creditID lets the
// provider pick the next available one. The outcome says whether anything was
// spent: nothing_to_reset keeps the credit.
func (s *Service) Reset(ctx context.Context, kind agent.Kind, creditID string) (string, error) {
	p, _ := agent.Find(s.providers, kind, Provider.Kind)
	r, ok := p.(Resetter)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNoResets, kind)
	}
	ctx, cancel := context.WithTimeout(ctx, resetTimeout)
	defer cancel()
	outcome, err := r.Reset(ctx, creditID)
	// Whatever happened, the cached windows and credits may be wrong now.
	s.mu.Lock()
	delete(s.fresh, kind)
	s.mu.Unlock()
	return outcome, err
}
