package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentusage"
	"github.com/repogoapp/host/internal/claudecode"
)

// A rate_limit_event as Claude Code 2.1 sends it with a turn.
const rateLimitEvent = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1796130000,"rateLimitType":"seven_day","overageStatus":"rejected","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.125,"resetsAt":1795700000,"status":"allowed_warning"},"seven_day":{"utilization":0.3,"resetsAt":1796130000,"status":"allowed"}}},"session_id":"s"}`

func TestLimitWindows(t *testing.T) {
	var m claudecode.Message
	if err := json.Unmarshal([]byte(rateLimitEvent), &m); err != nil {
		t.Fatal(err)
	}
	got := limitWindows(m.RateLimitInfo)
	if len(got) != 2 {
		t.Fatalf("windows = %+v", got)
	}
	if got[0].ID != "session" || got[0].Label != "5h" || got[0].WindowMinutes != 300 || got[0].UsedPercent != 12.5 ||
		got[0].ResetsAt != 1795700000 || got[0].Severity != "warning" || got[0].IsActive {
		t.Fatalf("5h = %+v", got[0])
	}
	if got[1].ID != "weekly_all" || got[1].Label != "Weekly" || got[1].UsedPercent != 30 || !got[1].IsActive {
		t.Fatalf("weekly = %+v", got[1])
	}

	// An older CLI names only the window that binds.
	u := 0.5
	only := limitWindows(&claudecode.RateLimitInfo{RateLimitType: "five_hour", Utilization: &u, ResetsAt: 10, Status: "rejected"})
	if len(only) != 1 || only[0].UsedPercent != 50 || !only[0].IsActive || only[0].Severity != "rejected" {
		t.Fatalf("binding only = %+v", only)
	}
	if limitWindows(&claudecode.RateLimitInfo{Status: "allowed"}) != nil || limitWindows(nil) != nil {
		t.Fatal("an event with no utilization gave windows")
	}
}

// A turn's event is what Usage serves, kept across a restart in the state
// folder, with the plan from the account file.
func TestUsageFromTurns(t *testing.T) {
	root := t.TempDir()
	deps := agent.Dependencies{Root: root, State: filepath.Join(root, "state")}
	p := New(deps)
	if got := p.Usage(context.Background(), agentusage.Usage{}); got.Available || len(got.Windows) != 0 {
		t.Fatalf("no files = %+v", got)
	}
	home := filepath.Join(root, "claude")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"organizationType":"claude_max"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := p.Usage(context.Background(), agentusage.Usage{}); !got.Available || got.Plan != "Max" || len(got.Windows) != 0 {
		t.Fatalf("plan only = %+v", got)
	}

	s := newLiveSession("s", root)
	s.limits = p.limits
	var m claudecode.Message
	if err := json.Unmarshal([]byte(rateLimitEvent), &m); err != nil {
		t.Fatal(err)
	}
	s.handleMessage(m)

	restarted := New(deps)
	got := restarted.Usage(context.Background(), agentusage.Usage{})
	if !got.Available || got.Plan != "Max" || len(got.Windows) != 2 || got.CapturedAtMS == 0 {
		t.Fatalf("after a turn = %+v", got)
	}
}

// A window read before its reset shows nothing used once it has reset.
func TestElapse(t *testing.T) {
	now := time.Unix(1000, 0)
	got := elapse([]agentusage.Window{
		{ID: "past", UsedPercent: 90, ResetsAt: 999, Severity: "warning"},
		{ID: "future", UsedPercent: 40, ResetsAt: 2000},
	}, now)
	if got[0].UsedPercent != 0 || got[0].Severity != "" || got[0].ResetsAt != 0 || got[1].UsedPercent != 40 {
		t.Fatalf("elapse = %+v", got)
	}
}

// get_usage's answer as Claude Code 2.1 gives it, trimmed: no 5-hour window,
// the account-wide weekly one and a model's own.
const getUsageAnswer = `{"subscription_type":"max","rate_limits_available":true,"rate_limits":{
	"five_hour":null,"seven_day":{"utilization":30,"resets_at":"2026-12-01T13:00:00.095207+00:00"},
	"limits":[
		{"kind":"weekly_all","group":"weekly","percent":30,"severity":"normal","resets_at":"2026-12-01T13:00:00.095207+00:00","scope":null,"is_active":true},
		{"kind":"weekly_scoped","group":"weekly","percent":25,"severity":"normal","resets_at":"2026-12-01T13:00:00.095334+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false}]}}`

func TestParseGetUsage(t *testing.T) {
	answer, ok := parseUsage(json.RawMessage(getUsageAnswer))
	if !ok || answer.Plan != "max" || !answer.Limits.Available {
		t.Fatalf("answer = %+v", answer)
	}
	got := answer.Limits.windows()
	if len(got) != 2 {
		t.Fatalf("windows = %+v", got)
	}
	if got[0].ID != "weekly_all" || got[0].Label != "Weekly" || got[0].UsedPercent != 30 || got[0].WindowMinutes != 10080 ||
		got[0].ResetsAt != 1796130000 || !got[0].IsActive {
		t.Fatalf("weekly = %+v", got[0])
	}
	if got[1].ID != "weekly_scoped:Fable" || got[1].Label != "Weekly · Fable" || got[1].UsedPercent != 25 || got[1].Scope != "Fable" {
		t.Fatalf("Fable = %+v", got[1])
	}

	// With no limits array, the top-level windows read the same.
	var fixed usageBody
	if err := json.Unmarshal([]byte(`{"five_hour":{"utilization":12.5,"resets_at":"2026-09-26T10:00:00Z"},"seven_day":{"utilization":55}}`), &fixed); err != nil {
		t.Fatal(err)
	}
	got = fixed.windows()
	if len(got) != 2 || got[0].ID != "session" || got[0].UsedPercent != 12.5 || got[0].ResetsAt != 1790416800 ||
		got[1].ID != "weekly_all" || got[1].UsedPercent != 55 {
		t.Fatalf("top-level = %+v", got)
	}
}

// A turn's event moves the windows it names and keeps a model's own cap.
func TestEventMergesIntoGetUsage(t *testing.T) {
	answer, _ := parseUsage(json.RawMessage(getUsageAnswer))
	l := &rateLimits{}
	l.store(answer.Limits.windows())
	u := 0.42
	l.record(&claudecode.RateLimitInfo{RateLimitType: "seven_day", UnifiedWindows: map[string]claudecode.RateLimitWindow{
		"seven_day": {Utilization: &u, ResetsAt: 1796130000, Status: "allowed"}}})
	got := l.load().Windows
	if len(got) != 2 || got[0].ID != "weekly_all" || got[0].UsedPercent != 42 || got[1].ID != "weekly_scoped:Fable" || got[1].UsedPercent != 25 {
		t.Fatalf("merged = %+v", got)
	}
}

// The account file's spellings name the plan as Claude's site does.
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
