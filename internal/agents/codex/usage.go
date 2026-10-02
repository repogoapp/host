package codex

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agentusage"
)

// Usage asks the CLI's account endpoint first: once a window is exhausted the
// CLI stops writing usable limits to its logs, exactly when the number matters.
// The rollouts come next, then last's windows.
func (p *Provider) Usage(ctx context.Context, last agentusage.Usage) agentusage.Usage {
	account := codexAccount(p.accountPath)
	if usage, ok := p.codexAccountUsage(ctx); ok {
		usage.Account = account
		return usage
	}
	if usage, ok := p.codexSessionLogUsage(); ok {
		usage.Account = account
		return usage
	}
	if len(last.Windows) > 0 {
		last.Detail = "codex unavailable; showing last known windows"
		// A credit listed then may be spent now; offer none rather than a stale one.
		last.Resets = nil
		return last
	}
	return agentusage.Usage{Account: account, Detail: "codex app-server unavailable and no windowed rate_limits in ~/.codex session logs"}
}

// codexAccountUsage reads account/rateLimits/read from a one-shot app-server.
func (p *Provider) codexAccountUsage(ctx context.Context) (agentusage.Usage, bool) {
	res, ok := appServer(ctx, map[string]string{"limits": "account/rateLimits/read"})
	raw := res["limits"]
	if !ok || len(raw) == 0 {
		return agentusage.Usage{}, false
	}
	return parseCodexAccountUsage(raw, time.Now())
}

// parseCodexAccountUsage reads the account endpoint's reply, reset credits
// included; false when it names no window.
func parseCodexAccountUsage(raw json.RawMessage, now time.Time) (agentusage.Usage, bool) {
	var body struct {
		RateLimits struct {
			PlanType             string          `json:"planType"`
			RateLimitReachedType string          `json:"rateLimitReachedType"`
			Primary              *codexWindowRaw `json:"primary"`
			Secondary            *codexWindowRaw `json:"secondary"`
		} `json:"rateLimits"`
		RateLimitResetCredits *codexResetsRaw `json:"rateLimitResetCredits"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return agentusage.Usage{}, false
	}

	usage := agentusage.Usage{
		Plan:             codexPlanLabel(body.RateLimits.PlanType),
		RateLimitReached: body.RateLimits.RateLimitReachedType,
		CapturedAtMS:     now.UnixMilli(),
		Detail:           "live from codex app-server account/rateLimits/read",
		Resets:           body.RateLimitResetCredits.resets(),
		Windows:          windows((*codexLogWindow)(body.RateLimits.Primary), (*codexLogWindow)(body.RateLimits.Secondary)),
	}
	// The logs never carry this, so it is the one thing the live read can say
	// that the fallback cannot: the provider itself calling you blocked.
	for i, w := range usage.Windows {
		if usage.RateLimitReached != "" && w.UsedPercent >= 100 {
			usage.Windows[i].Severity = "exceeded"
		}
	}

	if len(usage.Windows) == 0 {
		return agentusage.Usage{}, false
	}
	usage.Available = true
	return usage, true
}

// codexWindowRaw is one window as the account endpoint spells it; the logs'
// codexLogWindow is the same window in snake_case.
type codexWindowRaw struct {
	UsedPercent   float64 `json:"usedPercent"`
	WindowMinutes int64   `json:"windowDurationMins"`
	ResetsAt      int64   `json:"resetsAt"`
}

// windows is the primary and secondary windows, named by duration, never by
// field: `primary` can be the weekly window.
func windows(primary, secondary *codexLogWindow) []agentusage.Window {
	var out []agentusage.Window
	for _, spec := range []struct {
		raw      *codexLogWindow
		id, name string
	}{
		{primary, "primary", "5h"},
		{secondary, "secondary", "Weekly"},
	} {
		if spec.raw == nil {
			continue
		}
		out = append(out, agentusage.Window{
			ID:            spec.id,
			Label:         agentusage.WindowLabel(spec.raw.WindowMinutes, spec.name),
			UsedPercent:   spec.raw.UsedPercent,
			WindowMinutes: spec.raw.WindowMinutes,
			ResetsAt:      spec.raw.ResetsAt,
		})
	}
	return out
}

// codexSessionLogUsage recovers the newest windowed `rate_limits` block from
// the rollouts, newest file first and bottom-up, so the common case reads one
// file.
func (p *Provider) codexSessionLogUsage() (agentusage.Usage, bool) {
	snapshot, at, ok := p.latestCodexRateLimits()
	if !ok {
		return agentusage.Usage{}, false
	}

	usage := agentusage.Usage{
		Available:        true,
		Plan:             codexPlanLabel(snapshot.PlanType),
		RateLimitReached: snapshot.RateLimitReachedType,
		CapturedAtMS:     at.UnixMilli(),
		Detail:           "reconstructed from local Codex session logs, not official account billing",
		Windows:          windows(snapshot.Primary, snapshot.Secondary),
	}
	return usage, len(usage.Windows) > 0
}

type codexLogWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

type codexLogRateLimits struct {
	PlanType             string          `json:"plan_type"`
	RateLimitReachedType string          `json:"rate_limit_reached_type"`
	Primary              *codexLogWindow `json:"primary"`
	Secondary            *codexLogWindow `json:"secondary"`
}

// latestCodexRateLimits is the newest usable rate_limits block in any rollout.
func (p *Provider) latestCodexRateLimits() (codexLogRateLimits, time.Time, bool) {
	for _, path := range p.codexSessionFilesNewestFirst() {
		if limits, at, ok := lastRateLimitsIn(path); ok {
			return limits, at, true
		}
	}
	return codexLogRateLimits{}, time.Time{}, false
}

// codexSessionFilesNewestFirst lists live and archived rollouts by mtime.
func (p *Provider) codexSessionFilesNewestFirst() []string {
	type entry struct {
		path string
		mod  time.Time
	}
	var files []entry
	for _, dir := range []string{"sessions", "archived_sessions"} {
		root := filepath.Join(p.home, dir)
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			files = append(files, entry{path: path, mod: info.ModTime()})
			return nil
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })

	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.path)
	}
	return out
}

// lastRateLimitsIn finds the newest usable block in one file, read whole and
// walked backwards; only the newest rollout or two is ever touched.
func lastRateLimitsIn(path string) (codexLogRateLimits, time.Time, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return codexLogRateLimits{}, time.Time{}, false
	}
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		// Cheap reject before parsing: the overwhelming majority of rows in a
		// rollout are conversation, and unmarshalling each one to find out
		// would make this the most expensive thing in the request.
		if !strings.Contains(line, "rate_limits") {
			continue
		}
		var row struct {
			Timestamp string `json:"timestamp"`
			Payload   struct {
				RateLimits *codexLogRateLimits `json:"rate_limits"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &row) != nil || row.Payload.RateLimits == nil {
			continue
		}
		// An exhausted plan writes both windows as null. Keep walking back for
		// the last block that actually named one, rather than reporting a
		// snapshot that says nothing.
		limits := *row.Payload.RateLimits
		if limits.Primary == nil && limits.Secondary == nil {
			continue
		}
		at := time.UnixMilli(agent.UnixMillis(row.Timestamp))
		if at.UnixMilli() <= 0 {
			if info, err := os.Stat(path); err == nil {
				at = info.ModTime()
			}
		}
		return limits, at, true
	}
	return codexLogRateLimits{}, time.Time{}, false
}

// codexPlanLabel names a ChatGPT plan slug (app-server's PlanType); workspace
// variants fold into their plan.
func codexPlanLabel(slug string) string {
	switch slug {
	case "", "unknown":
		return ""
	case "pro":
		return "Pro 20x"
	case "prolite":
		return "Pro 5x"
	case "promax":
		return "Pro Max"
	case "self_serve_business_prolite", "self_serve_business_usage_based", "business":
		return "Business"
	case "ent26", "enterprise_cbp_automation", "enterprise_cbp_usage_based", "enterprise":
		return "Enterprise"
	case "edu", "edu_plus", "edu_pro":
		return "Edu"
	}
	return agentusage.PlanWords(slug)
}
