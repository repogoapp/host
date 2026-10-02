package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agentusage"
	"github.com/repogo/host/internal/clitool"
)

// Usage reads window utilization from Anthropic's OAuth usage endpoint, the
// only source since Claude Code persists none on disk, falling back to
// plan/tier from the local credential cache and last's windows.
func (p *Provider) Usage(ctx context.Context, last agentusage.Usage) agentusage.Usage {
	usage := p.claudePlanTierFromDisk()
	usage.Account = claudeAccount(p.accountPath)

	if live, ok := p.fetchClaudeUsage(ctx); ok {
		usage.Available = true
		usage.Windows = live
		usage.CapturedAtMS = time.Now().UnixMilli()
		usage.Detail = "live from api/oauth/usage (undocumented endpoint)"
		return usage
	}

	// Keep the disk-derived plan and serve the previous windows, aged.
	if len(last.Windows) > 0 {
		usage.Available = true
		usage.Windows = last.Windows
		usage.Account = last.Account
		usage.CapturedAtMS = last.CapturedAtMS
		usage.Detail = "usage endpoint unavailable; showing last known windows"
	}
	return usage
}

// claudePlanTierFromDisk is what we can say without the network: which plan
// this login is on. Worth having on its own — a popover that names the plan and
// admits it has no numbers beats one that renders nothing.
func (p *Provider) claudePlanTierFromDisk() agentusage.Usage {
	var usage agentusage.Usage
	var credentials struct {
		OAuth struct {
			Plan string `json:"subscriptionType"`
			Tier string `json:"rateLimitTier"`
		} `json:"claudeAiOauth"`
	}
	var config struct {
		Account struct {
			Plan string `json:"organizationType"`
			Tier string `json:"organizationRateLimitTier"`
		} `json:"oauthAccount"`
	}
	clitool.ReadJSON(filepath.Join(p.home, ".credentials.json"), &credentials)
	clitool.ReadJSON(p.accountPath, &config)

	plan := cmp.Or(credentials.OAuth.Plan, config.Account.Plan)
	tier := cmp.Or(credentials.OAuth.Tier, config.Account.Tier)
	if plan == "" && tier == "" {
		usage.Detail = "no local Claude credential cache"
		return usage
	}

	usage.Available = true
	usage.Plan, usage.Tier = claudePlanLabel(plan), tier
	usage.Detail = "plan/tier only; usage endpoint unavailable"
	return usage
}

// fetchClaudeUsage asks the usage endpoint with the Keychain's token.
func (p *Provider) fetchClaudeUsage(ctx context.Context) ([]agentusage.Window, bool) {
	token, ok := claudeKeychainToken(ctx)
	if !ok {
		return nil, false
	}

	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(
		reqCtx, http.MethodGet, "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer res.Body.Close()
	// 429 lands here and is treated as any other failure: the caller falls back
	// to the last good reading. See agentusage.Service's note — this endpoint
	// does rate limit its callers.
	if res.StatusCode != http.StatusOK {
		return nil, false
	}

	var body claudeUsageBody
	if json.NewDecoder(res.Body).Decode(&body) != nil {
		return nil, false
	}

	windows := body.windows()
	return windows, len(windows) > 0
}

// The endpoint answers in either of two shapes from one call to the next: a
// `limits` array, or top-level `five_hour`/`seven_day` objects.
type claudeUsageBody struct {
	Limits []struct {
		Kind     string   `json:"kind"`
		Group    string   `json:"group"`
		Percent  *float64 `json:"percent"`
		Severity string   `json:"severity"`
		ResetsAt string   `json:"resets_at"`
		IsActive bool     `json:"is_active"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`

	FiveHour *claudeFixedWindow `json:"five_hour"`
	SevenDay *claudeFixedWindow `json:"seven_day"`
}

// claudeFixedWindow is one window in the endpoint's top-level shape.
type claudeFixedWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// windows reads whichever shape the reply carried, `limits` first.
func (b claudeUsageBody) windows() []agentusage.Window {
	var out []agentusage.Window
	for _, limit := range b.Limits {
		if limit.Percent == nil {
			continue
		}
		w := agentusage.Window{
			ID:          limit.Kind,
			UsedPercent: *limit.Percent,
			Severity:    limit.Severity,
			IsActive:    limit.IsActive,
			ResetsAt:    agent.UnixMillis(limit.ResetsAt) / 1000,
		}
		if limit.Scope != nil && limit.Scope.Model != nil {
			w.Scope = limit.Scope.Model.DisplayName
		}
		switch limit.Kind {
		case "session":
			w.WindowMinutes, w.Label = 300, "5h"
		default:
			if limit.Group == "weekly" {
				w.WindowMinutes = 10080
			}
			w.Label = "Weekly"
			// Anthropic caps some models separately from the account-wide
			// weekly allowance, and two rows both labelled "Weekly" is a bug
			// report waiting to happen.
			if w.Scope != "" {
				w.Label = "Weekly · " + w.Scope
			}
		}
		out = append(out, w)
	}
	if len(out) > 0 {
		return out
	}
	for _, spec := range []struct {
		raw     *claudeFixedWindow
		id      string
		label   string
		minutes int64
	}{
		{b.FiveHour, "session", "5h", 300},
		{b.SevenDay, "weekly_all", "Weekly", 10080},
	} {
		if spec.raw == nil || spec.raw.Utilization == nil {
			continue
		}
		out = append(out, agentusage.Window{
			ID:            spec.id,
			Label:         spec.label,
			UsedPercent:   *spec.raw.Utilization,
			WindowMinutes: spec.minutes,
			ResetsAt:      agent.UnixMillis(spec.raw.ResetsAt) / 1000,
			// This shape does not say which window binds; 5h usually does.
			IsActive: spec.id == "session",
		})
	}
	return out
}

// claudeKeychainToken reads the current user's Keychain item, the token Claude
// Code refreshes; it never refreshes it itself.
func claudeKeychainToken(ctx context.Context) (string, bool) {
	if runtime.GOOS != "darwin" {
		return "", false
	}
	u, err := user.Current()
	if err != nil {
		return "", false
	}
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(probe, "security",
		"find-generic-password", "-s", keychainService, "-a", u.Username, "-w").Output()
	if err != nil {
		return "", false
	}

	var body struct {
		OAuth struct {
			AccessToken string   `json:"accessToken"`
			ExpiresAt   *float64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(string(out))), &body) != nil {
		return "", false
	}
	if body.OAuth.AccessToken == "" {
		return "", false
	}
	// Milliseconds. An expired token would just earn a 401 and a confusing
	// "endpoint unavailable"; checking here makes the degrade deliberate.
	if at := body.OAuth.ExpiresAt; at != nil && int64(*at/1000) <= time.Now().Unix() {
		return "", false
	}
	return body.OAuth.AccessToken, true
}

// claudePlanLabel names the plan as Claude's site does, "Max"; the two files
// spell it "max" (credentials) and "claude_max" (config).
func claudePlanLabel(plan string) string {
	slug := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(plan))
	slug = strings.TrimSuffix(strings.TrimPrefix(slug, "claude"), "subscription")
	switch {
	case slug == "":
		return ""
	case strings.HasPrefix(slug, "max"):
		return "Max"
	}
	return agentusage.PlanWords(strings.TrimPrefix(strings.ToLower(plan), "claude_"))
}
