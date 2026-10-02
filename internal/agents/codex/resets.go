package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/repogo/host/internal/agentusage"
)

// ErrResetFailed is Codex refusing or not answering the consume call.
var ErrResetFailed = errors.New("codex did not complete the reset")

// Reset outcomes, as Codex names them in snake_case.
const (
	resetDone            = "reset"
	resetNothingToReset  = "nothing_to_reset"
	resetNoCredit        = "no_credit"
	resetAlreadyRedeemed = "already_redeemed"
)

// Reset spends one of the account's rate-limit reset credits; an empty
// creditID lets Codex pick the next available one. The outcome says whether
// anything was spent: nothing_to_reset keeps the credit.
func (p *Provider) Reset(ctx context.Context, creditID string) (string, error) {
	// Fresh per call: each confirmed tap is its own reset.
	params := map[string]any{"idempotencyKey": uuid.NewString()}
	if creditID != "" {
		params["creditId"] = creditID
	}
	raw, ok := appServerCall(ctx, "account/rateLimitResetCredit/consume", params)
	if !ok {
		return "", ErrResetFailed
	}
	return parseResetOutcome(raw)
}

// parseResetOutcome maps Codex's camelCase outcome onto the snake_case
// name the host reports.
func parseResetOutcome(raw json.RawMessage) (string, error) {
	var body struct {
		Outcome string `json:"outcome"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Outcome == "" {
		return "", fmt.Errorf("%w: unreadable outcome %s", ErrResetFailed, raw)
	}
	switch body.Outcome {
	case "reset":
		return resetDone, nil
	case "nothingToReset":
		return resetNothingToReset, nil
	case "noCredit":
		return resetNoCredit, nil
	case "alreadyRedeemed":
		return resetAlreadyRedeemed, nil
	}
	// Unknown outcomes pass through.
	return body.Outcome, nil
}

// codexResetsRaw is `rateLimitResetCredits` from account/rateLimits/read.
// Credits is null when Codex knows only the count.
type codexResetsRaw struct {
	AvailableCount int64 `json:"availableCount"`
	Credits        []struct {
		ID          string  `json:"id"`
		Title       *string `json:"title"`
		Description *string `json:"description"`
		Status      string  `json:"status"`
		ResetType   string  `json:"resetType"`
		GrantedAt   int64   `json:"grantedAt"`
		ExpiresAt   *int64  `json:"expiresAt"`
	} `json:"credits"`
}

// resets is the credits in the shape the app reads; nil when Codex sent none.
func (r *codexResetsRaw) resets() *agentusage.Resets {
	if r == nil {
		return nil
	}
	out := &agentusage.Resets{Available: r.AvailableCount, Credits: []agentusage.ResetCredit{}}
	for _, c := range r.Credits {
		credit := agentusage.ResetCredit{
			ID:        c.ID,
			Status:    c.Status,
			ResetType: c.ResetType,
			GrantedAt: c.GrantedAt,
		}
		if c.Title != nil {
			credit.Title = *c.Title
		}
		if c.Description != nil {
			credit.Description = *c.Description
		}
		if c.ExpiresAt != nil {
			credit.ExpiresAt = *c.ExpiresAt
		}
		out.Credits = append(out.Credits, credit)
	}
	return out
}
