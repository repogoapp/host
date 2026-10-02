package chatlive

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogo/host/internal/agents/claude"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/testwait"
)

type fakeTurns struct{ ch chan agent.Event }

func (f *fakeTurns) Subscribe(string) (<-chan agent.Event, func(), error) {
	return f.ch, func() {}, nil
}

// streamCapture applies pushes the way the phone does: a push with an offset
// appends where the held text ends, one without replaces it. updates holds
// the text as applied after each push; raw holds the pushes as sent.
type streamCapture struct {
	mu      sync.Mutex
	updates []Streaming
	raw     []Streaming
	text    string
	gaps    int
}

func (c *streamCapture) Send(_ device.ID, method string, payload []byte) error {
	if method != "chats.streaming" {
		return nil
	}
	var p Streaming
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raw = append(c.raw, p)
	switch {
	case p.Offset == nil:
		c.text = p.Text
	case *p.Offset == len(c.text):
		c.text += p.Text
	default:
		c.gaps++
	}
	p.Text, p.Offset = c.text, nil
	c.updates = append(c.updates, p)
	return nil
}

func (c *streamCapture) sent() ([]Streaming, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Streaming(nil), c.raw...), c.gaps
}

// last is the newest snapshot, or the zero one.
func (c *streamCapture) last() Streaming {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.updates) == 0 {
		return Streaming{}
	}
	return c.updates[len(c.updates)-1]
}

func (c *streamCapture) all() []Streaming {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Streaming(nil), c.updates...)
}

func streamHarness(t *testing.T) (*Manager, *fakeTurns, *streamCapture, device.ID) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	turns := &fakeTurns{ch: make(chan agent.Event, 32)}
	cap := &streamCapture{}
	m := newManager(t, session.NewStore(claude.NewSessions(filepath.Join(t.TempDir(), ".claude"))), db, cap, turns)

	// A watcher for the chat, joined directly: Subscribe() would need a real
	// session file, and this test is about the stream, not the tail.
	caller := device.ID("00112233445566778899aabbccddeeff")
	m.emit.Join(caller, chatRoom("claude:s1"))

	return m, turns, cap, caller
}

// Each push carries the WHOLE message so far. A client that applies them in
// order, or misses some, ends up with the same text either way.
func TestStreamPushesCumulativeSnapshots(t *testing.T) {
	m, turns, cap, _ := streamHarness(t)
	m.StreamTurn("claude:s1", "turn-1")

	sent := ""
	for _, chunk := range []string{"Hello", " there", " world"} {
		turns.ch <- agent.Event{Kind: agent.EventText, Text: chunk}
		sent += chunk
		testwait.For(t, "a snapshot of "+sent, func() bool { return cap.last().Text == sent })
	}
	turns.ch <- agent.Event{Kind: agent.EventTurnFinished}
	testwait.For(t, "the closing snapshot", func() bool { return cap.last().Done })

	got := cap.all()
	if len(got) < 2 {
		t.Fatalf("expected several snapshots, got %d", len(got))
	}
	final := got[len(got)-1]
	if final.Text != "Hello there world" {
		t.Errorf("final text = %q", final.Text)
	}
	if !final.Done {
		t.Error("the last push should be marked done")
	}

	// Cumulative, never a fragment: each push must contain the previous one.
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1].Text, got[i].Text
		if len(cur) < len(prev) || cur[:len(prev)] != prev {
			t.Fatalf("push %d is not an extension of %d: %q then %q", i, i-1, prev, cur)
		}
	}
}

// Prose on either side of a tool call is two blocks in the transcript; the
// snapshot separates them the same way instead of gluing "first.Both".
func TestStreamBreaksParagraphAroundToolCalls(t *testing.T) {
	m, turns, cap, _ := streamHarness(t)
	m.StreamTurn("claude:s1", "turn-1")

	turns.ch <- agent.Event{Kind: agent.EventReasoning, Text: "hmm"}
	turns.ch <- agent.Event{Kind: agent.EventText, Text: "Let me look "}
	turns.ch <- agent.Event{Kind: agent.EventText, Text: "first."}
	turns.ch <- agent.Event{Kind: agent.EventToolCall, Tool: &agent.ToolCall{CallID: "t1", Name: "Read"}}
	turns.ch <- agent.Event{Kind: agent.EventToolResult, Tool: &agent.ToolCall{CallID: "t1"}}
	turns.ch <- agent.Event{Kind: agent.EventText, Text: "Both agents "}
	turns.ch <- agent.Event{Kind: agent.EventText, Text: "are exploring.\n"}
	turns.ch <- agent.Event{Kind: agent.EventToolCall, Tool: &agent.ToolCall{CallID: "t2", Name: "Bash"}}
	turns.ch <- agent.Event{Kind: agent.EventText, Text: "Done."}
	turns.ch <- agent.Event{Kind: agent.EventTurnFinished}

	testwait.For(t, "the closing snapshot", func() bool { return cap.last().Done })
	got := cap.all()
	if len(got) == 0 {
		t.Fatal("no snapshots")
	}
	// Leading thinking adds nothing; a text already ending in a newline is
	// not padded further.
	want := "Let me look first.\n\nBoth agents are exploring.\nDone."
	if final := got[len(got)-1].Text; final != want {
		t.Errorf("final text = %q, want %q", final, want)
	}
}

// Nothing should be sent to a device that is not watching this chat.
func TestStreamOnlyReachesWatchersOfThatChat(t *testing.T) {
	m, turns, cap, caller := streamHarness(t)

	m.emit.Leave(caller, chatRoom("claude:s1"))
	m.emit.Join(caller, chatRoom("claude:OTHER"))

	m.StreamTurn("claude:s1", "turn-1")
	turns.ch <- agent.Event{Kind: agent.EventText, Text: "secret"}
	turns.ch <- agent.Event{Kind: agent.EventTurnFinished}
	testwait.For(t, "the relay to end", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.streaming["claude:s1"] == 0
	})

	if got := cap.all(); len(got) != 0 {
		t.Errorf("a device watching another chat received %d stream pushes", len(got))
	}
}

// A device that subscribes mid-turn gets the turn's current snapshot in the
// reply, so it shows the header, the count and the stop button without
// waiting for the next token; once the turn closes the reply says so.
func TestSubscribeReturnsLiveState(t *testing.T) {
	_, sessions := liveHome(t, userLine("first"))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	metas, _ := sessions.List()
	if len(metas) == 0 {
		t.Fatal("session discovery found nothing in the temporary home")
	}

	cap := &streamCapture{}
	m := newManager(t, sessions, db, cap, &fakeTurns{})

	// The turn was opened by a hook while nobody was watching.
	at := time.UnixMilli(1_000_000)
	m.startDisplay("claude:live-1", "p1", at)
	m.noteDisplayWork("claude:live-1", "p1", at.Add(2*time.Second), true)
	if got := cap.all(); len(got) != 0 {
		t.Fatalf("pushed to nobody: %+v", got)
	}

	caller := device.ID("00112233445566778899aabbccddeeff")
	live, err := m.Subscribe(caller, "claude:live-1", 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer m.Unsubscribe(caller)

	if got := cap.all(); len(got) != 0 {
		t.Fatalf("subscribe pushed the state instead of returning it: %+v", got)
	}
	s := live.Streaming
	if s == nil || s.TurnID != "p1" || s.Done || s.StartedAt != at.UnixMilli() ||
		s.FirstFrameAt != at.Add(2*time.Second).UnixMilli() {
		t.Fatalf("live = %+v", live)
	}
	if live.Approval != nil || live.Epoch == "" || s.Epoch != live.Epoch || s.Revision > live.Revision {
		t.Errorf("live = %+v, streaming stamp %+v", live, s.Stamp)
	}

	// Once the turn closes the reply names no turn, at a newer revision than
	// the one that showed it.
	m.finishDisplay("claude:live-1", at.Add(5*time.Second), "", true)
	m.Unsubscribe(caller)
	after, err := m.Subscribe(caller, "claude:live-1", 0)
	if err != nil {
		t.Fatalf("resubscribe: %v", err)
	}
	if after.Streaming != nil || after.Revision <= live.Revision {
		t.Errorf("after close = %+v", after)
	}
	if b, _ := json.Marshal(after); !strings.Contains(string(b), `"streaming":null`) ||
		!strings.Contains(string(b), `"approval":null`) {
		t.Errorf("absence is not explicit on the wire: %s", b)
	}
}

// A frame read before Stop but arriving after it must not open the closed
// turn again: the room would hold a live snapshot for a turn that is over.
func TestDisplayFrameAfterStopDoesNotReopen(t *testing.T) {
	m, _, cap, caller := streamHarness(t)
	at := time.UnixMilli(1_000_000)
	m.startDisplay("claude:s1", "p1", at)
	m.finishDisplay("claude:s1", at.Add(time.Second), "", true)
	m.noteDisplayWork("claude:s1", "p1", at.Add(2*time.Second), true)
	m.startDisplay("claude:s1", "p1", at.Add(2*time.Second))

	if live := m.emit.Join(caller, chatRoom("claude:s1")); len(live.State) != 0 {
		t.Fatalf("closed turn reopened: %+v", live.State)
	}
	got := cap.all()
	if len(got) != 2 || !got[1].Done {
		t.Fatalf("pushes = %+v", got)
	}
	// A new prompt still opens.
	m.startDisplay("claude:s1", "p2", at.Add(3*time.Second))
	if live := m.emit.Join(caller, chatRoom("claude:s1")); live.State["streaming"].(Streaming).TurnID != "p2" {
		t.Fatalf("next turn did not open: %+v", live.State)
	}
}

// The relay announces a turn the moment it starts, so the device that just
// sent it sees something to stop before the first token.
func TestStreamAnnouncesTurnStart(t *testing.T) {
	m, turns, cap, _ := streamHarness(t)
	m.StreamTurn("claude:s1", "turn-1")
	turns.ch <- agent.Event{Kind: agent.EventTurnStarted, At: 1_000}

	testwait.For(t, "the start push", func() bool { return len(cap.all()) > 0 })
	got := cap.all()
	if len(got) != 1 || got[0].Done || got[0].Text != "" || got[0].StartedAt != 1_000 {
		t.Fatalf("start push = %+v", got)
	}
}

// Between whole snapshots a push carries only what the text gained, at the
// offset the phone's copy ends; the first and last push are whole.
func TestStreamSendsWhatTheTextGained(t *testing.T) {
	m, turns, cap, _ := streamHarness(t)
	m.StreamTurn("claude:s1", "turn-1")

	sent := ""
	for _, chunk := range []string{"Hello", " there", " — wörld"} {
		turns.ch <- agent.Event{Kind: agent.EventText, Text: chunk}
		sent += chunk
		testwait.For(t, "a snapshot of "+sent, func() bool { return cap.last().Text == sent })
	}
	turns.ch <- agent.Event{Kind: agent.EventTurnFinished}
	testwait.For(t, "the closing snapshot", func() bool { return cap.last().Done })

	raw, gaps := cap.sent()
	if gaps != 0 {
		t.Fatalf("%d pushes did not line up with the text held", gaps)
	}
	if raw[0].Offset != nil {
		t.Errorf("the first push is a delta: %+v", raw[0])
	}
	if last := raw[len(raw)-1]; last.Offset != nil || last.Text != sent {
		t.Errorf("the closing push is not the whole text: %+v", last)
	}
	deltas := 0
	for _, p := range raw[1 : len(raw)-1] {
		if p.Offset != nil && p.Text != "" {
			deltas++
			if strings.Contains(p.Text, "Hello") {
				t.Errorf("a delta resent earlier text: %q", p.Text)
			}
		}
	}
	if deltas == 0 {
		t.Errorf("no push was a delta: %+v", raw)
	}
}

// A whole snapshot goes out every fullEvery, so a device that missed a push
// catches up; a device joining mid-turn gets the whole text.
func TestStreamSendsTheWholeTextEveryFewSeconds(t *testing.T) {
	m, _, cap, _ := streamHarness(t)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }

	m.pushStream("claude:s1", "turn-1", "one", false, streamTiming{})
	m.pushStream("claude:s1", "turn-1", "one two", false, streamTiming{})
	now = now.Add(fullEvery)
	m.pushStream("claude:s1", "turn-1", "one two three", false, streamTiming{})
	m.pushStream("claude:s1", "turn-2", "new", false, streamTiming{})

	raw, _ := cap.sent()
	var shapes []string
	for _, p := range raw {
		if p.Offset == nil {
			shapes = append(shapes, "whole:"+p.Text)
		} else {
			shapes = append(shapes, fmt.Sprintf("at %d:%s", *p.Offset, p.Text))
		}
	}
	want := []string{"whole:one", "at 3: two", "whole:one two three", "whole:new"}
	if strings.Join(shapes, "|") != strings.Join(want, "|") {
		t.Errorf("pushes %v, want %v", shapes, want)
	}

	m.pushStream("claude:s1", "turn-2", "new text", false, streamTiming{})
	joined := m.emit.Join(device.ID("ffeeddccbbaa99887766554433221100"), chatRoom("claude:s1"))
	state := joined.State[Streaming{}.StateKey()].(Streaming)
	b, _ := json.Marshal(state)
	if state.Text != "new text" || strings.Contains(string(b), `"offset"`) {
		t.Errorf("a device joining mid-turn gets %s, want the whole text", b)
	}
}
