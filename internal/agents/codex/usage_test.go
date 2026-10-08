package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
)

// A window the account says is exhausted is marked exceeded; the rollouts
// never say so.
func TestAccountUsageMarksTheExhaustedWindow(t *testing.T) {
	raw := `{"rateLimits":{"rateLimitReachedType":"primary","primary":{"usedPercent":100,"windowDurationMins":300},"secondary":{"usedPercent":20,"windowDurationMins":10080}}}`
	usage, ok := parseCodexAccountUsage(json.RawMessage(raw), time.Now())
	if !ok || len(usage.Windows) != 2 || usage.Windows[0].Severity != "exceeded" || usage.Windows[1].Severity != "" ||
		usage.Windows[0].Label != "5h" || usage.Windows[1].Label != "Weekly" {
		t.Fatalf("usage = %+v", usage)
	}
}

// Bought credits are the balance; an account that never bought any, or has
// unlimited, has none to show.
func TestAccountUsageReadsTheCreditBalance(t *testing.T) {
	for credits, want := range map[string]float64{
		`{"hasCredits":true,"unlimited":false,"balance":"504.4950000000"}`: 504.495,
		`{"hasCredits":false,"unlimited":false,"balance":"0"}`:             -1,
		`{"hasCredits":true,"unlimited":true,"balance":null}`:              -1,
	} {
		raw := `{"rateLimits":{"primary":{"usedPercent":10,"windowDurationMins":300},"credits":` + credits + `}}`
		usage, ok := parseCodexAccountUsage(json.RawMessage(raw), time.Now())
		got := -1.0
		if usage.Balance != nil {
			got = usage.Balance.Remaining
			if usage.Balance.Unit != "credits" {
				t.Errorf("%s: unit %q", credits, usage.Balance.Unit)
			}
		}
		if !ok || got != want {
			t.Errorf("%s: balance %v, want %v", credits, got, want)
		}
	}
}

// With no app-server, the newest rollout's newest block that names a window
// is the reading; an exhausted plan's all-null block is skipped.
func TestSessionLogUsage(t *testing.T) {
	root := t.TempDir()
	p := New(agent.Dependencies{Root: root})
	if _, ok := p.codexSessionLogUsage(); ok {
		t.Fatal("a reading from no rollouts")
	}
	path := filepath.Join(root, "codex", "sessions", "2026", "09", "26", "rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	lines := `{"timestamp":"2026-09-26T09:00:00.5Z","payload":{"rate_limits":{"plan_type":"plus","primary":{"used_percent":40,"window_minutes":300,"resets_at":1790420000}}}}
{"timestamp":"2026-09-26T09:05:00Z","payload":{"rate_limits":{"primary":null,"secondary":null}}}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	usage, ok := p.codexSessionLogUsage()
	if !ok || usage.Plan != "Plus" || len(usage.Windows) != 1 || usage.Windows[0].UsedPercent != 40 ||
		usage.CapturedAtMS != time.Date(2026, 9, 26, 9, 0, 0, 5e8, time.UTC).UnixMilli() {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestCodexPlanLabel(t *testing.T) {
	for slug, want := range map[string]string{
		"plus": "Plus", "pro": "Pro 20x", "prolite": "Pro 5x", "team": "Team",
		"self_serve_business_prolite": "Business", "enterprise_cbp_automation": "Enterprise",
		"edu_plus": "Edu", "unknown": "", "": "", "new_plan": "New Plan",
	} {
		if got := codexPlanLabel(slug); got != want {
			t.Errorf("codexPlanLabel(%q) = %q, want %q", slug, got, want)
		}
	}
}
