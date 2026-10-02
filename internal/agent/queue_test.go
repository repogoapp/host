package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/testwait"
)

// savedQueues records every SaveQueue in the order the Manager made it.
type savedQueues struct {
	mu    sync.Mutex
	saves []savedQueue
}

type savedQueue struct {
	chatID string
	queued []StoredTurn
}

func (s *savedQueues) hooks() Hooks {
	hooks := quiet()
	hooks.SaveQueue = func(chatID string, queued []StoredTurn) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.saves = append(s.saves, savedQueue{chatID, queued})
		return nil
	}
	return hooks
}

func (s *savedQueues) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saves)
}

func (s *savedQueues) last() savedQueue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves[len(s.saves)-1]
}

func prompts(queued []StoredTurn) (out []string) {
	for _, q := range queued {
		out = append(out, q.Request.Prompt)
	}
	return out
}

// recordingAdapter runs each turn until cancelled and reports the request it
// ran. With a session, a new chat's first turn names it once nameSession closes.
type recordingAdapter struct {
	ran         chan TurnRequest
	session     string
	nameSession chan struct{}
}

func (recordingAdapter) Kind() Kind       { return KindClaude }
func (recordingAdapter) Available() error { return nil }
func (a recordingAdapter) Send(ctx context.Context, req TurnRequest, tio TurnIO) (Result, error) {
	a.ran <- req
	if req.SessionID == "" && a.session != "" {
		<-a.nameSession
		tio.Session(a.session)
	}
	<-ctx.Done()
	return Result{}, ctx.Err()
}

func (a recordingAdapter) next(t *testing.T) TurnRequest {
	t.Helper()
	select {
	case req := <-a.ran:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("no turn ran")
		return TurnRequest{}
	}
}

// Every change to a chat's queue is saved in order, with what a restart needs
// to run each turn, down to the empty queue after the last starts.
func TestEveryQueueChangeIsSaved(t *testing.T) {
	saved := &savedQueues{}
	adapter := recordingAdapter{ran: make(chan TurnRequest, 4)}
	m := NewManager(discard(), saved.hooks(), adapter)

	req := TurnRequest{ChatID: "claude:s1", Cwd: t.TempDir(), Agent: KindClaude, Prompt: "running",
		Config: TurnConfig{Model: "opus"}}
	running, err := m.Send(req)
	if err != nil {
		t.Fatal(err)
	}
	adapter.next(t)
	if saved.count() != 0 {
		t.Fatal("a turn that starts at once changed no queue")
	}
	req.Prompt = "b"
	b, _ := m.Send(req)
	req.Prompt = "c"
	req.Attachments = []Attachment{{Name: "shot.png", MimeType: "image/png", Path: "/tmp/shot.png"}}
	c, _ := m.Send(req)
	got := saved.last()
	if got.chatID != "claude:s1" || len(got.queued) != 2 || got.queued[1].TurnID != c.TurnID ||
		got.queued[1].Request.Config.Model != "opus" || len(got.queued[1].Request.Attachments) != 1 {
		t.Fatalf("after two enqueues = %+v", got)
	}

	if err := m.EditQueued(c.TurnID, "c, revised"); err != nil {
		t.Fatal(err)
	}
	if got := prompts(saved.last().queued); len(got) != 2 || got[1] != "c, revised" {
		t.Fatalf("after the edit = %v", got)
	}

	// A queue row acted on late must not stop the turn it became.
	if err := m.RemoveQueued(running.TurnID); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("queued-only stop of a running turn = %v", err)
	}
	if err := m.RemoveQueued(b.TurnID); err != nil {
		t.Fatal(err)
	}
	if got := prompts(saved.last().queued); len(got) != 1 || got[0] != "c, revised" {
		t.Fatalf("after dropping b = %v", got)
	}

	// Ending the running turn starts c and saves the empty queue.
	before := saved.count()
	if err := m.Stop(running.TurnID); err != nil {
		t.Fatal(err)
	}
	if ran := adapter.next(t); ran.Prompt != "c, revised" {
		t.Fatalf("ran %q next", ran.Prompt)
	}
	testwait.For(t, "the start to be saved", func() bool { return saved.count() > before })
	if got := saved.last(); len(got.queued) != 0 {
		t.Fatalf("after c started = %+v", got)
	}
	m.Stop(c.TurnID)
}

// Turns restored after a restart wait for Send Now: a new prompt runs past
// them, and Send Now runs one with the request it was queued with.
func TestRestoredTurnsWaitForSendNow(t *testing.T) {
	saved := &savedQueues{}
	adapter := recordingAdapter{ran: make(chan TurnRequest, 4)}
	m := NewManager(discard(), saved.hooks(), adapter)
	stored := TurnRequest{ChatID: "claude:s1", Cwd: t.TempDir(), Agent: KindClaude, Prompt: "restored",
		SessionID: "s1", Attachments: []Attachment{{Name: "shot.png", MimeType: "image/png", Path: "/tmp/shot.png"}}}
	m.Restore(map[string][]StoredTurn{
		"claude:s1": {{TurnID: "t-restored", QueuedAt: time.UnixMilli(1000), Request: stored}},
		// A new chat's queue under its temporary id has no chat to show it.
		"f3c1-temporary": {{TurnID: "t-orphan", QueuedAt: time.UnixMilli(1000), Request: stored}},
	})
	for _, s := range saved.saves {
		switch s.chatID {
		case "claude:s1":
			if len(s.queued) != 1 || !s.queued[0].Held {
				t.Fatalf("restored queue saved as %+v, want one held turn", s.queued)
			}
		case "f3c1-temporary":
			if len(s.queued) != 0 {
				t.Fatalf("a temporary id's queue was kept: %+v", s.queued)
			}
		}
	}

	fresh := stored
	fresh.Prompt, fresh.Attachments = "fresh", nil
	first, err := m.Send(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != StateRunning || adapter.next(t).Prompt != "fresh" {
		t.Fatal("a new prompt waited behind a held turn")
	}
	if _, err := m.SendQueued("t-restored", "phone"); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("Send Now while a turn runs = %v, want ErrInvalidTurn", err)
	}
	if err := m.Stop(first.TurnID); err != nil {
		t.Fatal(err)
	}
	testwait.For(t, "the chat to be idle", func() bool { return m.ActiveChats() == 0 })
	select {
	case req := <-adapter.ran:
		t.Fatalf("a held turn ran on its own: %q", req.Prompt)
	default:
	}

	sent, err := m.SendQueued("t-restored", device.ID("phone"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(sent.TurnID)
	ran := adapter.next(t)
	if ran.Prompt != "restored" || len(ran.Attachments) != 1 || ran.Device != "phone" {
		t.Fatalf("Send Now ran %+v", ran)
	}
	if got := saved.last(); got.chatID != "claude:s1" || len(got.queued) != 0 {
		t.Fatalf("queue after Send Now = %+v", got)
	}
	if _, err := m.SendQueued("t-restored", "phone"); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("Send Now twice = %v, want ErrNotQueued", err)
	}
}

// A turn queued behind a new chat's first turn moves to the chat the agent
// names, and continues that conversation rather than opening another.
func TestQueueBehindANewChatFollowsItsSession(t *testing.T) {
	saved := &savedQueues{}
	adapter := recordingAdapter{ran: make(chan TurnRequest, 4), session: "s-new", nameSession: make(chan struct{})}
	m := NewManager(discard(), saved.hooks(), adapter)
	req := TurnRequest{ChatID: "temp-1", Cwd: t.TempDir(), Agent: KindClaude, Prompt: "first"}
	first, err := m.Send(req)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(first.TurnID)
	adapter.next(t)
	req.Prompt = "second"
	second, err := m.Send(req)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(second.TurnID)

	close(adapter.nameSession)
	testwait.For(t, "the queue to move", func() bool { return saved.last().chatID == "claude:s-new" })
	got := saved.last().queued
	if len(got) != 1 || got[0].Request.ChatID != "claude:s-new" || got[0].Request.SessionID != "s-new" {
		t.Fatalf("queue after the rekey = %+v", got)
	}
}

// A host update stops running turns but holds the queued ones, so they wait
// in state.db for the restarted host instead of being dropped or started.
func TestStopAllHoldsQueuedTurns(t *testing.T) {
	saved := &savedQueues{}
	adapter := recordingAdapter{ran: make(chan TurnRequest, 4)}
	m := NewManager(discard(), saved.hooks(), adapter)
	req := TurnRequest{ChatID: "claude:s1", Cwd: t.TempDir(), Agent: KindClaude, Prompt: "running"}
	if _, err := m.Send(req); err != nil {
		t.Fatal(err)
	}
	adapter.next(t)
	req.Prompt = "waiting"
	if _, err := m.Send(req); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.StopAll(ctx); err != nil {
		t.Fatal(err)
	}
	got := saved.last()
	if len(got.queued) != 1 || got.queued[0].Request.Prompt != "waiting" || !got.queued[0].Held {
		t.Fatalf("queue after StopAll = %+v, want the waiting turn held", got.queued)
	}
	select {
	case req := <-adapter.ran:
		t.Fatalf("a queued turn started during the update: %q", req.Prompt)
	default:
	}
}
