package chatlive

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/repogo/host/internal/agents/claude"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
)

func frame(msg string, idx int, final bool, delta string) notify.DisplayFrame {
	return notify.DisplayFrame{
		Agent: agent.KindClaude, SessionID: "s1", PromptID: "p1",
		MessageID: msg, Index: idx, Final: final, Delta: delta, At: time.Now(),
	}
}

// A terminal turn's frames come out as the same cumulative snapshots a live
// turn produces, and Stop is what closes it.
func TestDisplayStreamsTerminalTurnAsSnapshots(t *testing.T) {
	m, _, cap, _ := streamHarness(t)

	m.Display(frame("m1", 0, false, "Hello"))
	m.Display(frame("m1", 1, true, ", world"))
	// Second message in the same turn (after a tool call): text keeps growing.
	m.Display(frame("m2", 0, true, " Done."))
	m.finishDisplay("claude:s1", time.Now(), "", true)

	got := cap.all()
	want := []Streaming{
		{ChatID: "claude:s1", TurnID: "p1", Text: "Hello"},
		{ChatID: "claude:s1", TurnID: "p1", Text: "Hello, world"},
		{ChatID: "claude:s1", TurnID: "p1", Text: "Hello, world Done."},
		{ChatID: "claude:s1", TurnID: "p1", Text: "Hello, world Done.", Done: true},
	}
	if len(got) != len(want) {
		t.Fatalf("pushes = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		// Timing is covered by TestDisplayOpensTurnOnPrompt, stamps by emit;
		// this is about text.
		got[i].streamTiming = streamTiming{}
		got[i].Stamp = emit.Stamp{}
		if got[i] != want[i] {
			t.Errorf("push %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, still := m.display["claude:s1"]; still {
		t.Error("turn state kept after finish")
	}
}

// Drops sort by second and pid, so a later frame can be read first. It must
// wait for its predecessor rather than land out of order.
func TestDisplayHoldsOutOfOrderFrames(t *testing.T) {
	m, _, cap, _ := streamHarness(t)

	m.Display(frame("m1", 1, false, "B"))
	m.Display(frame("m1", 0, false, "A"))
	m.Display(frame("m1", 2, true, "C"))

	texts := []string{}
	for _, p := range cap.all() {
		texts = append(texts, p.Text)
	}
	want := []string{"", "AB", "ABC"}
	if len(texts) != len(want) {
		t.Fatalf("texts = %q, want %q", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Errorf("push %d = %q, want %q", i, texts[i], want[i])
		}
	}
}

// A new prompt id without a Stop in between closes the previous turn first,
// so the client never sees two turns' text under one snapshot.
func TestDisplayNewPromptClosesPrevious(t *testing.T) {
	m, _, cap, _ := streamHarness(t)

	m.Display(frame("m1", 0, true, "first"))
	f := frame("m2", 0, true, "second")
	f.PromptID = "p2"
	m.Display(f)

	got := cap.all()
	if len(got) != 3 {
		t.Fatalf("pushes = %+v", got)
	}
	if !got[1].Done || got[1].TurnID != "p1" || got[1].Text != "first" {
		t.Errorf("previous turn not closed: %+v", got[1])
	}
	if got[2].TurnID != "p2" || got[2].Text != "second" || got[2].Done {
		t.Errorf("new turn wrong: %+v", got[2])
	}
}

// A turn this host runs already streams; the hook's copy of the same text
// would reset the client's snapshot under a different turn id.
func TestDisplayYieldsToLiveTurn(t *testing.T) {
	m, _, cap, _ := streamHarness(t)
	m.mu.Lock()
	m.streaming["claude:s1"] = 1
	m.mu.Unlock()

	m.Display(frame("m1", 0, true, "hook copy"))
	if got := cap.all(); len(got) != 0 {
		t.Fatalf("hook frames pushed during a live turn: %+v", got)
	}
}

// A device's turn whose hook frames are drained after the relay has exited:
// pushed, they would open a second turn repeating the reply.
func TestDisplayIgnoresAppOrigin(t *testing.T) {
	m, _, cap, _ := streamHarness(t)

	f := frame("m1", 0, true, "hook copy")
	f.Origin = notify.OriginApp
	m.Display(f)
	if got := cap.all(); len(got) != 0 {
		t.Fatalf("app-origin hook frames pushed: %+v", got)
	}
}

// A terminal turn goes live on UserPromptSubmit, before any text: the client
// needs the turn open to show tool calls arriving on the tail as live work.
func TestDisplayOpensTurnOnPrompt(t *testing.T) {
	m, _, cap, _ := streamHarness(t)
	at := time.UnixMilli(1_000_000)

	m.startDisplay("claude:s1", "p1", at)
	m.noteDisplayWork("claude:s1", "p1", at.Add(2*time.Second), true)
	m.noteDisplayWork("claude:s1", "p1", at.Add(3*time.Second), true) // second tool: no new push
	m.Display(frame("m1", 0, true, "Done."))
	m.finishDisplay("claude:s1", time.Now(), "", true)

	got := cap.all()
	if len(got) != 4 {
		t.Fatalf("pushes = %d: %+v", len(got), got)
	}
	if got[0].Text != "" || got[0].Done || got[0].StartedAt != at.UnixMilli() || got[0].FirstFrameAt != 0 {
		t.Errorf("prompt push = %+v", got[0])
	}
	if got[1].FirstFrameAt != at.Add(2*time.Second).UnixMilli() || got[1].Text != "" {
		t.Errorf("tool push = %+v", got[1])
	}
	if got[2].Text != "Done." || got[2].StartedAt != at.UnixMilli() || got[2].FirstFrameAt != got[1].FirstFrameAt {
		t.Errorf("text push lost timing = %+v", got[2])
	}
	if !got[3].Done || got[3].EndedAt == 0 {
		t.Errorf("close push = %+v", got[3])
	}
}

// A tool starting in a chat with no open turn — the host came up after the
// prompt — opens one, so a device watching sees the work rather than nothing.
func TestDisplayOpensTurnOnToolStart(t *testing.T) {
	m, _, cap, _ := streamHarness(t)
	at := time.UnixMilli(1_000_000)

	m.noteDisplayWork("claude:s1", "p1", at, false) // a tool finishing proves nothing
	m.noteDisplayWork("claude:s1", "p1", at, true)
	m.finishDisplay("claude:s1", at.Add(4*time.Second), "stopped", true)

	got := cap.all()
	if len(got) != 2 {
		t.Fatalf("pushes = %d: %+v", len(got), got)
	}
	if got[0].TurnID != "p1" || got[0].StartedAt != at.UnixMilli() || got[0].FirstFrameAt != at.UnixMilli() {
		t.Errorf("open push = %+v", got[0])
	}
	if !got[1].Done || got[1].EndedAt != at.Add(4*time.Second).UnixMilli() || got[1].StopReason != "stopped" {
		t.Errorf("close push = %+v", got[1])
	}
}

// A frame whose index is behind the folded prefix must not be kept, or the
// final-frame drain never reaches it and Display spins holding the mutex.
func TestDisplayLateFrameDoesNotHang(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := newManager(t, session.NewStore(claude.NewSessions(filepath.Join(t.TempDir(), ".claude"))), db, &streamCapture{}, &fakeTurns{})

	at := time.UnixMilli(1_000_000)
	frame := func(i int, delta string, final bool) notify.DisplayFrame {
		return notify.DisplayFrame{Agent: agent.KindClaude, SessionID: "s1", PromptID: "p1",
			MessageID: "m1", Index: i, Delta: delta, Final: final, At: at}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Display(frame(0, "a", false))
		m.Display(frame(1, "b", false))
		m.Display(frame(0, "a", false)) // replayed
		m.Display(frame(3, "d", true))  // 2 never arrives
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Display hung on a late frame")
	}
	m.mu.Lock()
	text := m.display["claude:s1"].text.String()
	m.mu.Unlock()
	if text != "abd" {
		t.Errorf("text %q, want abd", text)
	}
}
