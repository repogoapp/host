package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/repogo/host/internal/testwait"
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
