package chatlive

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/testwait"
)

type attentionSink struct {
	mu  sync.Mutex
	got []Attention
}

func (s *attentionSink) add(a Attention) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, a)
}

func (s *attentionSink) all() []Attention {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Attention(nil), s.got...)
}

// wait returns once n attentions arrived, or fails.
func (s *attentionSink) wait(t *testing.T, n int) []Attention {
	t.Helper()
	testwait.For(t, fmt.Sprintf("%d attentions", n), func() bool { return len(s.all()) >= n })
	return s.all()
}

func attentionHarness(t *testing.T) (*Manager, *fakeTurns, *attentionSink) {
	t.Helper()
	m, turns, _, _ := streamHarness(t)
	sink := &attentionSink{}
	m.onAttention = sink.add
	return m, turns, sink
}

// An approval, its answer and the turn's end each reach the sink, answerable,
// and the end carries what the turn said.
func TestAttentionFollowsAnAppTurn(t *testing.T) {
	m, turns, sink := attentionHarness(t)
	m.StreamTurn("claude:s1", "turn-1")

	approval := &agent.Approval{CallID: "c1", Title: "Run ls", Kind: "execute",
		Options: []agent.ApprovalOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}}
	turns.ch <- agent.Event{Kind: agent.EventApprovalRequested, Approval: approval}
	turns.ch <- agent.Event{Kind: agent.EventApprovalResolved, Approval: &agent.Approval{CallID: "c1", Title: `"allow"`}}
	turns.ch <- agent.Event{Kind: agent.EventText, Text: "All done."}
	turns.ch <- agent.Event{Kind: agent.EventTurnFinished}

	got := sink.wait(t, 3)
	if got[0].Kind != AttentionApproval || got[0].Approval == nil || got[0].Approval.CallID != "c1" ||
		got[0].ChatID != "claude:s1" || got[0].TurnID != "turn-1" || !got[0].Answerable {
		t.Errorf("approval = %+v", got[0])
	}
	if got[1].Kind != AttentionResolved || got[1].CallID != "c1" || got[1].Approval != nil {
		t.Errorf("resolved = %+v", got[1])
	}
	if got[2].Kind != AttentionFinished || got[2].Reply != "All done." || !got[2].Answerable {
		t.Errorf("finished = %+v", got[2])
	}
}

func TestAttentionNamesHowATurnEnded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event agent.Event
		kind  string
		err   string
	}{
		{"stopped", agent.Event{Kind: agent.EventTurnFinished, Error: "stopped"}, AttentionStopped, ""},
		{"failed", agent.Event{Kind: agent.EventTurnFailed, Error: "rate limited"}, AttentionFailed, "rate limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, turns, sink := attentionHarness(t)
			m.StreamTurn("claude:s1", "turn-1")
			turns.ch <- tc.event
			got := sink.wait(t, 1)
			if got[0].Kind != tc.kind || got[0].Error != tc.err {
				t.Errorf("attention = %+v", got[0])
			}
		})
	}
}

// A long turn sends its closing words, not its opening ones.
func TestAttentionClipsTheReplyToItsEnd(t *testing.T) {
	long := strings.Repeat("é", maxAttentionReply) + " the end."
	got := clipReply(long)
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "the end.") {
		t.Errorf("clipped = %q", got)
	}
	if n := len([]rune(got)); n > maxAttentionReply+1 {
		t.Errorf("clipped to %d runes", n)
	}
	if clipReply("  short \n") != "short" {
		t.Error("short text should pass through trimmed")
	}
}

// A terminal turn's prompt is spoken but not answerable, and the tool running
// after it resolves it.
func TestAttentionFromTerminalHooks(t *testing.T) {
	m, _, sink := attentionHarness(t)
	base := notify.Notice{Agent: agent.KindClaude, SessionID: "s1", Origin: notify.OriginCLI, PromptID: "p1"}

	perm := base
	perm.Event, perm.ToolName, perm.ToolUseID = "PermissionRequest", "Bash", "tu1"
	m.attendHook(perm, "")

	post := base
	post.Event, post.ToolName, post.ToolUseID = "PostToolUse", "Bash", "tu1"
	m.attendHook(post, "")
	// A second tool with nothing pending says nothing.
	m.attendHook(post, "")

	ask := base
	ask.Event, ask.ToolName, ask.ToolUseID = "PreToolUse", "AskUserQuestion", "tu2"
	ask.ToolInput = json.RawMessage(`{"questions":[{"question":"Which one?","header":"Pick","options":[{"label":"A"},{"label":"B"}]}]}`)
	ask.Questions = notify.Questions(ask.ToolInput)
	m.attendHook(ask, "")

	stop := base
	stop.Event, stop.Status = "Stop", agent.ChatCompleted
	m.attendHook(stop, "Finished it.")

	got := sink.all()
	if len(got) != 4 {
		t.Fatalf("attentions = %+v", got)
	}
	if got[0].Kind != AttentionApproval || got[0].Answerable || got[0].Approval.CallID != "tu1" ||
		got[0].ChatID != "claude:s1" || got[0].TurnID != "p1" {
		t.Errorf("permission = %+v", got[0])
	}
	if got[1].Kind != AttentionResolved || got[1].CallID != "tu1" {
		t.Errorf("resolved = %+v", got[1])
	}
	q := got[2].Approval
	if got[2].Kind != AttentionApproval || q == nil || q.Kind != "question" || len(q.Questions) != 1 ||
		q.Questions[0].Text != "Which one?" || len(q.Questions[0].Options) != 2 {
		t.Errorf("question = %+v", got[2])
	}
	if got[3].Kind != AttentionFinished || got[3].Reply != "Finished it." || got[3].Answerable {
		t.Errorf("finished = %+v", got[3])
	}

	// The app's own turns come through the relay, not their hooks.
	app := stop
	app.Origin = notify.OriginApp
	m.attendHook(app, "")
	if n := len(sink.all()); n != 4 {
		t.Errorf("an app turn's hook produced attention: %d", n)
	}
}

type twoPeers struct{ ids []device.ID }

func (p twoPeers) Identity() *device.Identity { return &device.Identity{ID: "host-1"} }
func (p twoPeers) Peers() []device.Peer {
	out := make([]device.Peer, len(p.ids))
	for i, id := range p.ids {
		out[i] = device.Peer{ID: id}
	}
	return out
}

type sentCapture struct {
	mu   sync.Mutex
	sent map[device.ID][]string
}

func (c *sentCapture) Send(to device.ID, method string, _ []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent[to] = append(c.sent[to], method)
	return nil
}

// Attention is not a room's: every paired device hears it, watching the chat
// or not.
func TestAttentionReachesEveryPairedDevice(t *testing.T) {
	a, b := device.ID("aa"), device.ID("bb")
	cap := &sentCapture{sent: map[device.ID][]string{}}
	NewAnnouncer(twoPeers{ids: []device.ID{a, b}}, emit.New(cap, slog.Default()), nil, noLabel, slog.Default()).
		Attention(Attention{ChatID: "claude:s1", Kind: AttentionFinished})
	for _, id := range []device.ID{a, b} {
		if got := cap.sent[id]; len(got) != 1 || got[0] != "chats.attention" {
			t.Errorf("%s got %v", id, got)
		}
	}
}

// A store change reaches every paired device as the row it names now reads,
// or as a removal; a row gone since the commit is skipped.
func TestAnnounceSendsRowsAndRemovalsToEveryPairedDevice(t *testing.T) {
	a, b := device.ID("aa"), device.ID("bb")
	cap := &sentCapture{sent: map[device.ID][]string{}}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetStatus("claude:s1", "/tmp/s1", agent.ChatIdle, time.Now(), store.TurnObservation{}); err != nil {
		t.Fatal(err)
	}
	NewAnnouncer(twoPeers{ids: []device.ID{a, b}}, emit.New(cap, slog.Default()), db, noLabel, slog.Default()).
		Announce(store.Change{Changed: []store.ChatID{"claude:s1", "claude:gone"}, Removed: []store.ChatID{"claude:s2"}})
	for _, id := range []device.ID{a, b} {
		if got := cap.sent[id]; len(got) != 2 || got[0] != "chats.changed" || got[1] != "chats.removed" {
			t.Errorf("%s got %v", id, got)
		}
	}
}

func noLabel(agent.Kind, string) string { return "" }
