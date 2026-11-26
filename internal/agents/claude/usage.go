package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentusage"
	"github.com/repogoapp/host/internal/claudecode"
	"github.com/repogoapp/host/internal/clitool"
)

// Claude's limits are what Claude Code itself reports, with its own sign-in:
// get_usage, the Agent SDK's control request, answered by a throwaway claude
// with no prompt; and between those, the rate_limit_event each chat turn's
// stream carries. The host never reads Claude's token or asks Anthropic itself.

// rateLimits is the latest reading, kept in the state folder so a restart
// still shows it.
type rateLimits struct {
	mu   sync.Mutex
	path string
	read bool
	last storedLimits
}

type storedLimits struct {
	CapturedAtMS int64               `json:"captured_at_ms"`
	Windows      []agentusage.Window `json:"windows"`
}

// Usage asks Claude Code for the account's limits, else serves the latest
// reading (a turn's event or an earlier answer), aged by when it was taken.
func (p *Provider) Usage(ctx context.Context, last agentusage.Usage) agentusage.Usage {
	usage := agentusage.Usage{Plan: p.claudePlan()}
	account, _ := claudeAccount(p.accountPath)
	usage.Account = agentusage.AccountOf(account)

	// A test host never reads the user's own account.
	if p.deps.Root == "" {
		if answer, ok := getUsage(ctx); ok {
			usage.Plan = cmp.Or(claudePlanLabel(answer.Plan), usage.Plan)
			usage.Available = true
			if windows := answer.Limits.windows(); len(windows) > 0 {
				p.limits.store(windows)
			} else if !answer.Limits.Available {
				// An API key has no subscription windows.
				return usage
			}
		}
	}

	stored := p.limits.load()
	if len(stored.Windows) == 0 && len(last.Windows) > 0 {
		stored = storedLimits{CapturedAtMS: last.CapturedAtMS, Windows: last.Windows}
	}
	usage.Available = usage.Available || usage.Plan != "" || len(stored.Windows) > 0
	if len(stored.Windows) > 0 {
		usage.CapturedAtMS = stored.CapturedAtMS
		usage.Windows = elapse(stored.Windows, time.Now())
	}
	return usage
}

// usageAnswer is get_usage's answer, the parts the host shows.
type usageAnswer struct {
	Plan   string    `json:"subscription_type"`
	Limits usageBody `json:"-"`
}

// getUsage asks a throwaway claude for get_usage.
func getUsage(ctx context.Context) (usageAnswer, bool) {
	// No settings: the account is all get_usage needs, and skipping the
	// user's settings, plugins and MCP servers answers in about 2 s, not 2.7.
	res, ok := streamJSONControl(ctx, map[string]string{"usage": "get_usage"}, "--setting-sources", "")
	if !ok || res["usage"] == nil {
		return usageAnswer{}, false
	}
	return parseUsage(res["usage"])
}

func parseUsage(raw json.RawMessage) (usageAnswer, bool) {
	var answer usageAnswer
	var body struct {
		Available  bool      `json:"rate_limits_available"`
		RateLimits usageBody `json:"rate_limits"`
	}
	if json.Unmarshal(raw, &answer) != nil || json.Unmarshal(raw, &body) != nil {
		return usageAnswer{}, false
	}
	answer.Limits = body.RateLimits
	answer.Limits.Available = body.Available
	return answer, true
}

// usageBody is get_usage's rate_limits: a `limits` array, and the same
// windows as top-level objects. Percentages are 0 to 100.
type usageBody struct {
	Available bool `json:"-"`
	Limits    []struct {
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

	FiveHour *fixedWindow `json:"five_hour"`
	SevenDay *fixedWindow `json:"seven_day"`
}

type fixedWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// windows reads the `limits` array, else the top-level windows.
func (b usageBody) windows() []agentusage.Window {
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
			// Some models have a weekly cap of their own beside the
			// account-wide one: name the model, and keep each its own row.
			if w.Scope != "" {
				w.Label = "Weekly · " + w.Scope
				w.ID = limit.Kind + ":" + w.Scope
			}
		}
		out = append(out, w)
	}
	if len(out) > 0 {
		return out
	}
	for _, spec := range []struct {
		raw *fixedWindow
		id  string
	}{{b.FiveHour, "five_hour"}, {b.SevenDay, "seven_day"}} {
		if spec.raw == nil || spec.raw.Utilization == nil {
			continue
		}
		s := limitSpecs[spec.id]
		out = append(out, agentusage.Window{
			ID: s.id, Label: s.label, WindowMinutes: s.minutes,
			UsedPercent: *spec.raw.Utilization,
			ResetsAt:    agent.UnixMillis(spec.raw.ResetsAt) / 1000,
		})
	}
	return out
}

// claudePlan is the account's plan as Claude's account file names it.
func (p *Provider) claudePlan() string {
	var config struct {
		Account struct {
			Plan string `json:"organizationType"`
		} `json:"oauthAccount"`
	}
	clitool.ReadJSON(p.accountPath, &config)
	return claudePlanLabel(config.Account.Plan)
}

// record updates the windows a turn's rate_limit_event names, keeping the
// ones it doesn't (a model's own weekly cap) as get_usage last read them.
func (l *rateLimits) record(info *claudecode.RateLimitInfo) {
	windows := limitWindows(info)
	if len(windows) == 0 {
		return
	}
	l.load()
	l.mu.Lock()
	merged := slices.Clone(l.last.Windows)
	for _, w := range windows {
		if i := slices.IndexFunc(merged, func(m agentusage.Window) bool { return m.ID == w.ID }); i >= 0 {
			merged[i] = w
		} else {
			merged = append(merged, w)
		}
	}
	l.mu.Unlock()
	l.store(merged)
}

// store keeps windows as the latest reading.
func (l *rateLimits) store(windows []agentusage.Window) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.read = true
	l.last = storedLimits{CapturedAtMS: time.Now().UnixMilli(), Windows: windows}
	if l.path == "" {
		return
	}
	if data, err := json.Marshal(l.last); err == nil {
		_ = os.MkdirAll(filepath.Dir(l.path), 0o700)
		_ = os.WriteFile(l.path, data, 0o600)
	}
}

// load is the latest reading, from the state file the first time.
func (l *rateLimits) load() storedLimits {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.read && l.path != "" {
		l.read = true
		clitool.ReadJSON(l.path, &l.last)
	}
	return l.last
}

// limitWindows reads an event's windows: every one it carried, else the one
// that binds when an older CLI names only that. Utilization is 0 to 1.
func limitWindows(info *claudecode.RateLimitInfo) []agentusage.Window {
	if info == nil {
		return nil
	}
	var out []agentusage.Window
	add := func(name string, w claudecode.RateLimitWindow) {
		spec, ok := limitSpecs[name]
		if !ok || w.Utilization == nil {
			return
		}
		out = append(out, agentusage.Window{
			ID: spec.id, Label: spec.label, WindowMinutes: spec.minutes,
			UsedPercent: min(*w.Utilization*100, 100),
			ResetsAt:    w.ResetsAt,
			Severity:    limitSeverity(w.Status),
			IsActive:    name == info.RateLimitType,
		})
	}
	for _, name := range []string{"five_hour", "seven_day"} {
		if w, ok := info.UnifiedWindows[name]; ok {
			add(name, w)
		}
	}
	if len(out) == 0 {
		add(info.RateLimitType, claudecode.RateLimitWindow{Utilization: info.Utilization, ResetsAt: info.ResetsAt, Status: info.Status})
	}
	return out
}

// limitSpecs are the account-wide windows, by the name the event and
// get_usage's top-level objects give them; IDs match get_usage's kinds.
var limitSpecs = map[string]struct {
	id, label string
	minutes   int64
}{
	"five_hour": {"session", "5h", 300},
	"seven_day": {"weekly_all", "Weekly", 10080},
}

// limitSeverity is the apps' word for an event window's status.
func limitSeverity(status string) string {
	switch status {
	case "allowed_warning":
		return "warning"
	case "rejected":
		return "rejected"
	}
	return ""
}

// elapse empties the windows that have reset since they were read.
func elapse(windows []agentusage.Window, now time.Time) []agentusage.Window {
	out := make([]agentusage.Window, len(windows))
	for i, w := range windows {
		if w.ResetsAt > 0 && w.ResetsAt <= now.Unix() {
			w.UsedPercent, w.Severity, w.ResetsAt = 0, "", 0
		}
		out[i] = w
	}
	return out
}

// claudePlanLabel names the plan as Claude's site does, "Max"; get_usage
// says "max" and the account file "claude_max".
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
