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
	span := UserSpan{ID: "s1", Kind: ActivityUser, Cwd: "/w/app", StartedAtMs: ms(0), EndedAtMs: ms(10)}
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
		{ID: "ok", Kind: ActivityUser, Cwd: "/w/app", StartedAtMs: ms(0), EndedAtMs: ms(5)},
		{ID: "", Kind: ActivityUser, Cwd: "/w/app", StartedAtMs: ms(0), EndedAtMs: ms(5)},
		{ID: "relative", Kind: ActivityUser, Cwd: "w/app", StartedAtMs: ms(0), EndedAtMs: ms(5)},
		{ID: "backwards", Kind: ActivityUser, Cwd: "/w/app", StartedAtMs: ms(5), EndedAtMs: ms(0)},
		{ID: "too-long", Kind: ActivityUser, Cwd: "/w/app", StartedAtMs: ms(0), EndedAtMs: ms(25 * 60)},
		{ID: "future", Kind: ActivityUser, Cwd: "/w/app", StartedAtMs: ms(10), EndedAtMs: ms(20)},
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

// Prompt spans carry what they were; one without a known input, a screen-time
// span with one, or a kind a device can't send (or none) is skipped. Clearing removes
// prompting with screen time.
func TestPromptSpansKeepTheirInput(t *testing.T) {
	l := openTestLedger(t, t.TempDir(), rates{})
	ctx := context.Background()
	now := t0.Add(time.Hour)
	spans := []UserSpan{
		{ID: "screen", Kind: ActivityUser, Cwd: "/w/app", StartedAtMs: ms(0), EndedAtMs: ms(30)},
		{ID: "typed", Kind: ActivityPrompt, Input: "typing", Cwd: "/w/app", StartedAtMs: ms(2), EndedAtMs: ms(3)},
		{ID: "spoke", Kind: ActivityPrompt, Input: "voice", Cwd: "/w/app", StartedAtMs: ms(5), EndedAtMs: ms(6)},
		{ID: "no-input", Kind: ActivityPrompt, Cwd: "/w/app", StartedAtMs: ms(7), EndedAtMs: ms(8)},
		{ID: "odd-input", Kind: ActivityPrompt, Input: "humming", Cwd: "/w/app", StartedAtMs: ms(7), EndedAtMs: ms(8)},
		{ID: "screen-input", Kind: ActivityUser, Input: "typing", Cwd: "/w/app", StartedAtMs: ms(7), EndedAtMs: ms(8)},
		{ID: "agent", Kind: ActivityAgent, Cwd: "/w/app", StartedAtMs: ms(7), EndedAtMs: ms(8)},
		{ID: "no-kind", Cwd: "/w/app", StartedAtMs: ms(7), EndedAtMs: ms(8)},
	}
	if skipped, err := l.RecordUser(ctx, phone, spans, now); err != nil || skipped != 5 {
		t.Fatalf("skipped %d, %v; want 5", skipped, err)
	}
	got := spansOf(t, l, Scope{}, now)
	if len(got) != 3 || got[0].Kind != ActivityUser || got[0].Input != "" ||
		got[1].Kind != ActivityPrompt || got[1].Input != "typing" || got[2].Input != "voice" {
		t.Fatalf("spans = %+v, want screen, typed, spoke", got)
	}
	// The same ID can't change kind on a later upload.
	if _, err := l.RecordUser(ctx, phone, []UserSpan{{ID: "screen", Kind: ActivityPrompt, Input: "typing",
		Cwd: "/w/app", StartedAtMs: ms(0), EndedAtMs: ms(40)}}, now); err != nil {
		t.Fatal(err)
	}
	if got := spansOf(t, l, Scope{}, now); got[0].Kind != ActivityUser || got[0].EndedAtMs != ms(30) {
		t.Fatalf("screen span became %+v", got[0])
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
func TestActivityScopes(t *testing.T) {
	l := openTestLedger(t, t.TempDir(), rates{})
	ctx := context.Background()
	if _, err := l.RecordUser(ctx, phone, []UserSpan{
		{ID: "app", Kind: ActivityUser, Cwd: "/w/app", StartedAtMs: ms(0), EndedAtMs: ms(5)},
		{ID: "sub", Kind: ActivityUser, Cwd: "/w/app/sub", StartedAtMs: ms(10), EndedAtMs: ms(15)},
		{ID: "sibling", Kind: ActivityUser, Cwd: "/w/app2", StartedAtMs: ms(0), EndedAtMs: ms(5)},
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
}
