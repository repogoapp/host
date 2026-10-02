package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/agent"
)

// The usage endpoint answers with a `limits` array or with top-level windows,
// from one call to the next; both read the same.
func TestUsageBodyShapes(t *testing.T) {
	var limits claudeUsageBody
	if err := json.Unmarshal([]byte(`{"limits":[
		{"kind":"session","percent":12.5,"resets_at":"2026-09-26T10:00:00Z","is_active":true},
		{"kind":"weekly_all","group":"weekly","percent":40,"resets_at":"2026-09-30T00:00:00Z"},
		{"kind":"weekly_opus","group":"weekly","percent":90,"scope":{"model":{"display_name":"Opus"}}},
		{"kind":"no_percent"}]}`), &limits); err != nil {
		t.Fatal(err)
	}
	got := limits.windows()
	if len(got) != 3 || got[0].Label != "5h" || got[0].WindowMinutes != 300 || got[0].ResetsAt != 1790416800 || !got[0].IsActive {
		t.Fatalf("limits shape = %+v", got)
	}
	if got[1].Label != "Weekly" || got[1].WindowMinutes != 10080 || got[2].Label != "Weekly · Opus" || got[2].Scope != "Opus" {
		t.Fatalf("weekly windows = %+v", got[1:])
	}

	var fixed claudeUsageBody
	if err := json.Unmarshal([]byte(`{"five_hour":{"utilization":30,"resets_at":"2026-09-26T10:00:00Z"},"seven_day":{"utilization":55}}`), &fixed); err != nil {
		t.Fatal(err)
	}
	got = fixed.windows()
	if len(got) != 2 || got[0].ID != "session" || got[0].UsedPercent != 30 || got[0].ResetsAt != 1790416800 ||
		got[1].ID != "weekly_all" || got[1].UsedPercent != 55 || got[1].IsActive {
		t.Fatalf("top-level shape = %+v", got)
	}
}

// With no endpoint, the plan still comes from the credential cache, and the
// account file fills what the cache leaves out.
func TestPlanTierFromDisk(t *testing.T) {
	root := t.TempDir()
	p := New(agent.Dependencies{Root: root})
	if got := p.claudePlanTierFromDisk(); got.Available || got.Detail != "no local Claude credential cache" {
		t.Fatalf("no files = %+v", got)
	}
	home := filepath.Join(root, "claude")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte(`{"claudeAiOauth":{"subscriptionType":"max"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"organizationType":"team","organizationRateLimitTier":"tier_4"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := p.claudePlanTierFromDisk(); !got.Available || got.Plan != "Max" || got.Tier != "tier_4" {
		t.Fatalf("plan/tier = %+v", got)
	}
}

// Both files' spellings name the plan as Claude's site does.
func TestClaudePlanLabel(t *testing.T) {
	for plan, want := range map[string]string{
		"claude_max": "Max", "max": "Max", "claude_max_20x": "Max", "claude_pro": "Pro", "pro": "Pro",
		"claude_team": "Team", "claude_enterprise": "Enterprise", "claude_something_new": "Something New", "": "",
	} {
		if got := claudePlanLabel(plan); got != want {
			t.Errorf("claudePlanLabel(%q) = %q, want %q", plan, got, want)
		}
	}
}
