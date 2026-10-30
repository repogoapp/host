package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/testwait"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// quiet is Hooks that hear nothing.
func quiet() Hooks {
	return Hooks{Session: func(TurnStatus) {}, Status: func(TurnStatus, ChatStatus, time.Time) {},
		SaveQueue: func(string, []StoredTurn) error { return nil }}
}

// blockingAdapter runs a turn until its context is cancelled.
type blockingAdapter struct{}

func (blockingAdapter) Kind() Kind       { return KindClaude }
func (blockingAdapter) Available() error { return nil }
func (blockingAdapter) Send(ctx context.Context, _ TurnRequest, _ TurnIO) (Result, error) {
	<-ctx.Done()
	return Result{}, ctx.Err()
}

// A client shows "queued" from the send reply, so the reply must say running
// for a turn that starts at once and queued only for one behind another.
func TestSendReportsRunningUnlessBehindAnotherTurn(t *testing.T) {
	m := NewManager(discard(), quiet(), blockingAdapter{})
	req := TurnRequest{ChatID: "claude:s1", Cwd: t.TempDir(), Agent: KindClaude, Prompt: "first"}

	first, err := m.Send(req)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(first.TurnID)
	if first.State != StateRunning {
		t.Fatalf("first turn in an idle chat reported %q, want %q", first.State, StateRunning)
	}

	req.Prompt = "second"
	second, err := m.Send(req)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(second.TurnID)
	if second.State != StateQueued {
		t.Fatalf("turn behind a running one reported %q, want %q", second.State, StateQueued)
	}
}

// askingAdapter blocks on one approval, then finishes.
type askingAdapter struct{}

func (askingAdapter) Kind() Kind       { return KindClaude }
func (askingAdapter) Available() error { return nil }
func (askingAdapter) Send(ctx context.Context, _ TurnRequest, io TurnIO) (Result, error) {
	_, err := io.Ask(ctx, Approval{CallID: "c1", Title: "Run ls"})
	return Result{}, err
}

// A device that starts listening late (voice) learns the prompt a turn is
// blocked on from turns.list, and it is gone once answered.
func TestTurnStatusCarriesPendingApproval(t *testing.T) {
	m := NewManager(discard(), quiet(), askingAdapter{})
	st, err := m.Send(TurnRequest{ChatID: "claude:s1", Cwd: t.TempDir(), Agent: KindClaude, Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(st.TurnID)

	pending := func() *Approval { got, _ := m.Status(st.TurnID); return got.Approval }
	testwait.For(t, "the approval", func() bool { return pending() != nil })
	if got := pending(); got.CallID != "c1" {
		t.Fatalf("pending approval = %+v", got)
	}
	if err := m.Respond(st.TurnID, "c1", json.RawMessage(`"allow"`)); err != nil {
		t.Fatal(err)
	}
	testwait.For(t, "the answered approval to clear", func() bool { return pending() == nil })
}

// A queued turn's prompt can change while it waits and keeps its place; once
// it has started the edit is refused, so a late edit never re-runs a turn.
func TestEditQueuedChangesAWaitingTurnAndRefusesAStartedOne(t *testing.T) {
	saved := &savedQueues{}
	m := NewManager(discard(), saved.hooks(), blockingAdapter{})
	req := TurnRequest{ChatID: "claude:s1", Cwd: t.TempDir(), Agent: KindClaude, Prompt: "first"}

	first, err := m.Send(req)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(first.TurnID)
	req.Prompt = "second"
	second, err := m.Send(req)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(second.TurnID)
	req.Prompt = "third"
	third, err := m.Send(req)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(third.TurnID)

	if err := m.EditQueued(second.TurnID, "second, revised"); err != nil {
		t.Fatal(err)
	}
	last := saved.last().queued
	if len(last) != 2 || last[0].TurnID != second.TurnID || last[0].Request.Prompt != "second, revised" {
		t.Fatalf("queue after the edit = %+v, want the revised turn first", last)
	}
	if err := m.EditQueued(first.TurnID, "too late"); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("editing the running turn: err = %v, want ErrNotQueued", err)
	}
	if err := m.EditQueued(second.TurnID, " "); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("empty prompt: err = %v, want ErrInvalidTurn", err)
	}
	if err := m.EditQueued("missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown turn: err = %v, want ErrNotFound", err)
	}
}

// steeringAdapter runs a turn until cancelled and takes steered input unless
// it refuses, as Codex does in plan mode.
type steeringAdapter struct {
	blockingAdapter
	refuse  bool
	steered chan string
}

func (a steeringAdapter) Steer(_ context.Context, turnID string, req TurnRequest) error {
	if a.refuse {
		return errors.New("no active turn to steer")
	}
	a.steered <- turnID + ":" + req.Prompt
	return nil
}

// A steer that did not land must queue and say so: the phone shows the
// message as sent into the turn only when steered is true.
func TestSteerJoinsTheRunningTurnOrQueues(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		adapter := steeringAdapter{refuse: refuse, steered: make(chan string, 1)}
		m := NewManager(discard(), quiet(), adapter)
		req := TurnRequest{ChatID: "claude:s1", Cwd: t.TempDir(), Agent: KindClaude, Prompt: "first"}

		first, steered, err := m.Steer(req)
		if err != nil || steered || first.State != StateRunning {
			t.Fatalf("steer into an idle chat: %#v steered=%v: %v", first, steered, err)
		}
		req.Prompt = "answer"
		second, steered, err := m.Steer(req)
		if err != nil {
			t.Fatal(err)
		}
		if refuse {
			if steered || second.State != StateQueued || second.TurnID == first.TurnID {
				t.Fatalf("refused steer %#v steered=%v, want a queued turn", second, steered)
			}
		} else {
			if !steered || second.TurnID != first.TurnID {
				t.Fatalf("steer %#v steered=%v, want the running turn", second, steered)
			}
			if got := <-adapter.steered; got != first.TurnID+":answer" {
				t.Fatalf("adapter steered %q", got)
			}
		}
		m.Stop(second.TurnID)
		m.Stop(first.TurnID)
	}
}

func TestSteerQueuesForAnAgentThatCannot(t *testing.T) {
	m := NewManager(discard(), quiet(), blockingAdapter{})
	req := TurnRequest{ChatID: "claude:s1", Cwd: t.TempDir(), Agent: KindClaude, Prompt: "first"}
	first, err := m.Send(req)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(first.TurnID)
	second, steered, err := m.Steer(req)
	if err != nil || steered || second.State != StateQueued {
		t.Fatalf("got %#v steered=%v: %v", second, steered, err)
	}
	m.Stop(second.TurnID)
}

// joiningAdapter runs turns until stopped, and keeps the Manager's adopt so a
// test can start a turn as the agent would by itself.
type joiningAdapter struct {
	blockingAdapter
	adopt *func(chatID, cwd string)
}

func (a joiningAdapter) OwnTurns(adopt func(chatID, cwd string)) { *a.adopt = adopt }
func (a joiningAdapter) Join(ctx context.Context, _ string, io TurnIO) (Result, error) {
	io.Session("own", "")
	<-ctx.Done()
	return Result{}, ctx.Err()
}

// A turn the agent starts by itself runs as the Manager's: it has an id, holds
// a send in the queue behind it, and Stop on the chat ends it.
func TestAdoptedTurnQueuesSendsAndStops(t *testing.T) {
	var adopt func(chatID, cwd string)
	sessions := make(chan TurnStatus, 4)
	hooks := quiet()
	hooks.Session = func(st TurnStatus) { sessions <- st }
	m := NewManager(discard(), hooks, joiningAdapter{adopt: &adopt})
	cwd := t.TempDir()

	adopt("claude:own", cwd)
	own := <-sessions
	if own.ChatID != "claude:own" || own.TurnID == "" {
		t.Fatalf("adopted turn %#v", own)
	}
	adopt("claude:own", cwd)
	if n := len(m.List()); n != 1 {
		t.Fatalf("adopting a running chat again made %d turns", n)
	}

	next, err := m.Send(TurnRequest{ChatID: "claude:own", Cwd: cwd, Agent: KindClaude, Prompt: "after"})
	if err != nil || next.State != StateQueued {
		t.Fatalf("send behind the adopted turn: %#v: %v", next, err)
	}
	if err := m.StopChat("claude:own"); err != nil {
		t.Fatal(err)
	}
	testwait.For(t, "the queued turn to start", func() bool {
		status, _ := m.Status(next.TurnID)
		return status.State == StateRunning
	})
	if status, _ := m.Status(own.TurnID); status.State != StateStopped {
		t.Fatalf("adopted turn %s, want stopped", status.State)
	}
	m.Stop(next.TurnID)

	if err := m.StopChat("claude:idle"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("idle chat: %v, want ErrNotFound", err)
	}
}

// Stopping a chat stops the chats its agent started, their queued turns
// included, and leaves every other chat running. A parent with nothing of its
// own running still stops its children.
func TestStopChatStopsItsChildChats(t *testing.T) {
	m := NewManager(discard(), quiet(), blockingAdapter{})
	cwd := t.TempDir()
	send := func(chatID, parent string) TurnStatus {
		t.Helper()
		st, err := m.Send(TurnRequest{ChatID: chatID, Cwd: cwd, Agent: KindClaude, Prompt: "go", Parent: parent})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	parent := send("claude:parent", "")
	child := send("claude:child", "claude:parent")
	childNext := send("claude:child", "claude:parent")
	stranger := send("claude:stranger", "")
	defer m.Stop(stranger.TurnID)
	if childNext.State != StateQueued {
		t.Fatalf("second child turn %s, want queued", childNext.State)
	}

	if err := m.StopChat("claude:parent"); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"parent": parent.TurnID, "child": child.TurnID, "queued child": childNext.TurnID} {
		testwait.For(t, name+" to stop", func() bool {
			st, _ := m.Status(id)
			return st.State == StateStopped
		})
	}
	if st, _ := m.Status(stranger.TurnID); st.State != StateRunning {
		t.Fatalf("unrelated chat %s, want still running", st.State)
	}

	orphan := send("claude:child2", "claude:idle-parent")
	if err := m.StopChat("claude:idle-parent"); err != nil {
		t.Fatalf("parent with only a child running: %v, want nil", err)
	}
	testwait.For(t, "the idle parent's child to stop", func() bool {
		st, _ := m.Status(orphan.TurnID)
		return st.State == StateStopped
	})
}

// replyingAdapter says something, calls a tool, answers, then finishes once
// release closes.
type replyingAdapter struct{ release chan struct{} }

func (replyingAdapter) Kind() Kind       { return KindClaude }
func (replyingAdapter) Available() error { return nil }
func (a replyingAdapter) Send(ctx context.Context, _ TurnRequest, io TurnIO) (Result, error) {
	io.Emit(Event{Kind: EventText, Text: "Looking."})
	io.Emit(Event{Kind: EventToolCall})
	io.Emit(Event{Kind: EventToolResult})
	io.Emit(Event{Kind: EventText, Text: "Found "})
	io.Emit(Event{Kind: EventText, Text: "it."})
	select {
	case <-a.release:
		return Result{}, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Wait returns at its deadline with the turn still running, and once the turn
// ends with the agent's last message: the text after its last tool call.
func TestWaitReturnsTheLastMessageWhenTheTurnEnds(t *testing.T) {
	release := make(chan struct{})
	m := NewManager(discard(), quiet(), replyingAdapter{release: release})
	st, err := m.Send(TurnRequest{ChatID: "claude:s1", Cwd: t.TempDir(), Agent: KindClaude, Prompt: "find it"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(st.TurnID)

	expired, cancel := context.WithCancel(t.Context())
	cancel()
	status, _, err := m.Wait(expired, st.TurnID)
	if err != nil || status.EndedAt != nil {
		t.Fatalf("a wait past its deadline: ended %v, err %v; want the turn still running", status.EndedAt, err)
	}

	close(release)
	status, reply, err := m.Wait(t.Context(), st.TurnID)
	if err != nil || status.State != StateDone || reply != "Found it." {
		t.Fatalf("Wait = %q, %q, %v; want done with the last message", status.State, reply, err)
	}
	if _, _, err := m.Wait(t.Context(), "no-such-turn"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("waiting on an unknown turn: %v, want ErrNotFound", err)
	}
}

// openingAdapter opens a new conversation running model, then waits.
type openingAdapter struct{ model string }

func (openingAdapter) Kind() Kind       { return KindCodex }
func (openingAdapter) Available() error { return nil }
func (a openingAdapter) Send(ctx context.Context, _ TurnRequest, io TurnIO) (Result, error) {
	io.Session("t1", a.model)
	<-ctx.Done()
	return Result{}, ctx.Err()
}

// A chat sent with the CLI's default model, or an alias, names the model the
// agent runs from the status its row is first written with.
func TestSessionModelFillsTheDefault(t *testing.T) {
	for _, tc := range []struct{ requested, want string }{{"", "gpt-6-astra"}, {"gpt-6", "gpt-6-astra"}} {
		statuses := make(chan TurnStatus, 8)
		hooks := quiet()
		hooks.Status = func(st TurnStatus, _ ChatStatus, _ time.Time) { statuses <- st }
		m := NewManager(discard(), hooks, openingAdapter{model: "gpt-6-astra"})
		st, err := m.Send(TurnRequest{ChatID: "codex:new", Cwd: t.TempDir(), Agent: KindCodex, Prompt: "hi",
			Config: TurnConfig{Model: tc.requested}})
		if err != nil {
			t.Fatal(err)
		}
		first := <-statuses
		if first.SessionID != "t1" || first.Model != tc.want {
			t.Fatalf("requested %q: first status %q model %q, want t1 with %q", tc.requested, first.SessionID, first.Model, tc.want)
		}
		m.Stop(st.TurnID)
	}
}

// lateModelAdapter opens the conversation, then learns its model, as Claude
// and ACP agents do once the turn's settings are applied.
type lateModelAdapter struct{}

func (lateModelAdapter) Kind() Kind       { return KindClaude }
func (lateModelAdapter) Available() error { return nil }
func (lateModelAdapter) Send(ctx context.Context, _ TurnRequest, io TurnIO) (Result, error) {
	io.Session("s1", "")
	io.Session("s1", "claude-opus-5-5")
	<-ctx.Done()
	return Result{}, ctx.Err()
}

// A model learned after the session opened reaches the chat row, replacing
// the alias the device asked for.
func TestLateSessionModelUpdatesTheStatus(t *testing.T) {
	statuses := make(chan TurnStatus, 8)
	hooks := quiet()
	hooks.Status = func(st TurnStatus, _ ChatStatus, _ time.Time) { statuses <- st }
	m := NewManager(discard(), hooks, lateModelAdapter{})
	st, err := m.Send(TurnRequest{ChatID: "claude:new", Cwd: t.TempDir(), Agent: KindClaude, Prompt: "hi", Config: TurnConfig{Model: "opus"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(st.TurnID)
	if first := <-statuses; first.Model != "opus" {
		t.Fatalf("first status model %q, want the alias asked for", first.Model)
	}
	if second := <-statuses; second.SessionID != "s1" || second.Model != "claude-opus-5-5" {
		t.Fatalf("second status %q model %q, want s1 with claude-opus-5-5", second.SessionID, second.Model)
	}
}
