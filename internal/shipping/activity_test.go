package shipping

import (
	"context"
	"testing"
	"time"
)

var (
	phone  = Device{ID: "phone", Label: "Jerrick's iPhone", Platform: "ios"}
	tablet = Device{ID: "tablet", Label: "iPad", Platform: "ipados"}
	t0     = time.UnixMilli(1_793_534_400_000)
)

func ms(minutes int) int64 { return t0.Add(time.Duration(minutes) * time.Minute).UnixMilli() }

func spansOf(t *testing.T, l *Ledger, scope Scope, now time.Time) []Span {
	t.Helper()
	spans, err := l.Activity(context.Background(), ms(-600), ms(600), scope, now)
	if err != nil {
		t.Fatal(err)
	}
	return spans
}

// A device's span is stored once; sending it again only moves its end forward,
// never back, and another device can't touch it.
func TestRecordUserMovesOnlyItsOwnEndForward(t *testing.T) {
	l := openTestLedger(t, t.TempDir(), rates{})
	ctx := context.Background()
	span := UserSpan{ID: "s1", Cwd: "/w/app", StartedAtMs: ms(0), EndedAtMs: ms(10)}
	for _, step := range []struct {
		device Device
		end    int
	}{{phone, 10}, {phone, 25}, {phone, 15}, {tablet, 90}} {
		span.EndedAtMs = ms(step.end)
		if skipped, err := l.RecordUser(ctx, step.device, []UserSpan{span}, t0.Add(time.Hour)); err != nil || skipped != 0 {
			t.Fatalf("record: %d skipped, %v", skipped, err)
		}
	}
	got := spansOf(t, l, Scope{}, t0)
	if len(got) != 1 || got[0].EndedAtMs != ms(25) || got[0].Actor != "phone" || got[0].Label != "Jerrick's iPhone" ||
		got[0].Kind != ActivityUser || got[0].Platform != "ios" {
		t.Fatalf("spans = %+v, want one phone span ending at +25m", got)
	}
}

// Spans that can't be right are skipped and the rest kept.
func TestRecordUserSkipsImpossibleSpans(t *testing.T) {
	l := openTestLedger(t, t.TempDir(), rates{})
	now := t0
	spans := []UserSpan{
		{ID: "ok", Cwd: "/w/app", StartedAtMs: ms(0), EndedAtMs: ms(5)},
		{ID: "", Cwd: "/w/app", StartedAtMs: ms(0), EndedAtMs: ms(5)},
		{ID: "relative", Cwd: "w/app", StartedAtMs: ms(0), EndedAtMs: ms(5)},
		{ID: "backwards", Cwd: "/w/app", StartedAtMs: ms(5), EndedAtMs: ms(0)},
		{ID: "too-long", Cwd: "/w/app", StartedAtMs: ms(0), EndedAtMs: ms(25 * 60)},
		{ID: "future", Cwd: "/w/app", StartedAtMs: ms(10), EndedAtMs: ms(20)},
	}
	skipped, err := l.RecordUser(context.Background(), phone, spans, now)
	if err != nil || skipped != 5 {
		t.Fatalf("skipped %d, %v; want 5", skipped, err)
	}
	if got := spansOf(t, l, Scope{}, now); len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("spans = %+v, want only ok", got)
	}
	if _, err := l.RecordUser(context.Background(), phone, make([]UserSpan, maxSpanBatch+1), now); err == nil {
		t.Fatal("an oversized batch was accepted")
	}
}

// The agent clock: working opens, a wait closes, an answer opens again on the
// same turn, and a finish closes. A repeated status changes nothing, a new turn
// closes the old span, and a status with no folder or turn opens nothing.
func TestAgentStatusRunsTheClock(t *testing.T) {
	l := openTestLedger(t, t.TempDir(), rates{})
	ctx := context.Background()
	status := func(working bool, turn string, minute int) {
		t.Helper()
		if err := l.AgentStatus(ctx, AgentStatus{ChatID: "claude:c1", Agent: "claude", Cwd: "/w/app",
			TurnID: turn, Working: working, At: t0.Add(time.Duration(minute) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	status(true, "t1", 0)
	status(true, "t1", 2) // the same status again
	status(false, "t1", 4)
	status(false, "t1", 5) // still waiting
	status(true, "t1", 14) // the user answered
	status(true, "t2", 16) // a new turn
	status(false, "t2", 20)

	got := spansOf(t, l, Scope{}, t0.Add(time.Hour))
	want := [][2]int{{0, 4}, {14, 16}, {16, 20}}
	if len(got) != len(want) {
		t.Fatalf("spans = %+v, want %v", got, want)
	}
	for i, w := range want {
		if got[i].StartedAtMs != ms(w[0]) || got[i].EndedAtMs != ms(w[1]) || got[i].Kind != ActivityAgent || got[i].ChatID != "claude:c1" {
			t.Fatalf("span %d = %+v, want %v", i, got[i], w)
		}
	}
	if got[0].TurnID != "t1" || got[1].TurnID != "t1" || got[2].TurnID != "t2" {
		t.Fatalf("turns = %s %s %s", got[0].TurnID, got[1].TurnID, got[2].TurnID)
	}

	if err := l.AgentStatus(ctx, AgentStatus{ChatID: "codex:c2", Agent: "codex", TurnID: "t9", Working: true, At: t0}); err != nil {
		t.Fatal(err)
	}
	if n := len(spansOf(t, l, Scope{}, t0.Add(time.Hour))); n != 3 {
		t.Fatalf("a status with no folder opened a span: %d spans", n)
	}
}

// An open agent span reads as ending now; a stopped host's spans end at their
// chat's last sign of life, or at their start when there is none.
func TestOpenAgentSpansEndAtNowAndCloseAtLastSeen(t *testing.T) {
	l := openTestLedger(t, t.TempDir(), rates{})
	ctx := context.Background()
	for _, chat := range []string{"claude:seen", "claude:unseen"} {
		if err := l.AgentStatus(ctx, AgentStatus{ChatID: chat, Agent: "claude", Cwd: "/w/app", TurnID: "t", Working: true, At: t0}); err != nil {
			t.Fatal(err)
		}
	}
	if got := spansOf(t, l, Scope{}, t0.Add(30*time.Minute)); got[0].EndedAtMs != ms(30) {
		t.Fatalf("open span ends at %d, want now", got[0].EndedAtMs)
	}
	n, err := l.CloseOpenAgents(ctx, func(chat string) (time.Time, bool) {
		return t0.Add(7 * time.Minute), chat == "claude:seen"
	})
	if err != nil || n != 2 {
		t.Fatalf("closed %d, %v; want 2", n, err)
	}
	ends := map[string]int64{}
	for _, s := range spansOf(t, l, Scope{}, t0.Add(time.Hour)) {
		ends[s.ChatID] = s.EndedAtMs
	}
	if ends["claude:seen"] != ms(7) || ends["claude:unseen"] != ms(0) {
		t.Fatalf("ends = %v, want seen at +7m and unseen at its start", ends)
	}
}

// Reads take both kinds in a scope; clearing removes only devices' time.
func TestActivityScopesAndClearKeepsAgentWork(t *testing.T) {
	l := openTestLedger(t, t.TempDir(), rates{})
	ctx := context.Background()
	if _, err := l.RecordUser(ctx, phone, []UserSpan{
		{ID: "app", Cwd: "/w/app", StartedAtMs: ms(0), EndedAtMs: ms(5)},
		{ID: "sub", Cwd: "/w/app/sub", StartedAtMs: ms(10), EndedAtMs: ms(15)},
		{ID: "sibling", Cwd: "/w/app2", StartedAtMs: ms(0), EndedAtMs: ms(5)},
	}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := l.AgentStatus(ctx, AgentStatus{ChatID: "claude:c", Agent: "claude", Cwd: "/w/app", TurnID: "t", Working: true, At: t0}); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(time.Hour)
	if got := spansOf(t, l, Folders("/w/app"), now); len(got) != 3 {
		t.Fatalf("/w/app: %d spans, want app, sub and the agent's", len(got))
	}
	if got := spansOf(t, l, Folders(), now); len(got) != 0 {
		t.Fatalf("no folders: %d spans, want none", len(got))
	}
	if err := l.ClearUser(ctx, Folders("/w/app")); err != nil {
		t.Fatal(err)
	}
	got := spansOf(t, l, Scope{}, now)
	if len(got) != 2 || got[0].Kind == got[1].Kind {
		t.Fatalf("after clearing /w/app: %+v, want the sibling and the agent's span", got)
	}
}
