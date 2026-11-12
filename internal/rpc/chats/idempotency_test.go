package chats_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/chat"
	"github.com/repogoapp/host/internal/idempotency"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/rpc/chats"
	"github.com/repogoapp/host/internal/store"
	"github.com/repogoapp/host/internal/testhost"
)

// counting is a sender that numbers the turns it starts.
type counting struct {
	sender
	runs int
}

func (c *counting) Send(req agent.TurnRequest) (agent.TurnStatus, error) {
	c.runs++
	c.last = req
	return agent.TurnStatus{TurnID: "turn-" + string(rune('0'+c.runs)), State: agent.StateRunning}, nil
}

// turnStates is the manager's view: the turns it still holds.
type turnStates map[string]agent.TurnState

func (t turnStates) Status(id string) (agent.TurnStatus, error) {
	state, ok := t[id]
	if !ok {
		return agent.TurnStatus{}, agent.ErrNotFound
	}
	return agent.TurnStatus{TurnID: id, State: state}, nil
}

type keyed struct {
	r     *rpc.Router
	rec   *counting
	turns turnStates
	db    *store.Store
	root  string
}

func keyedRouter(t *testing.T) keyed {
	t.Helper()
	th := testhost.New(t)
	d := th.ChatDeps
	rec := &counting{}
	d.Sender = rec
	svc, err := chat.New(d)
	if err != nil {
		t.Fatal(err)
	}
	turns := turnStates{}
	r := rpc.New(slog.New(slog.DiscardHandler))
	chats.Register(r, chats.Deps{
		Chats: svc, Live: th.Config.Live,
		Keys: idempotency.New(th.Config.Store), Turns: turns, TurnChats: th.Config.Store,
	})
	return keyed{r, rec, turns, th.Config.Store, th.Root}
}

var phone = rpc.Caller{Device: "phone", Scope: rpc.ScopeRemote}

func (k keyed) start(t *testing.T, c rpc.Caller, prompt, key string) (chats.StartResult, error) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"cwd": k.root, "agent": "codex", "turn": map[string]any{"prompt": prompt}, "idempotency_key": key,
	})
	raw, err := k.r.Call(context.Background(), c, "chats.start", b)
	var out chats.StartResult
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}

// A start a phone resends after a lost reply starts one chat, and the
// repeat reads the turn's state as it is now, not as it was.
func TestAResentStartRunsOnceAndReadsTheTurnNow(t *testing.T) {
	k := keyedRouter(t)
	first, err := k.start(t, phone, "review this", "key-1")
	if err != nil || first.TurnID != "turn-1" || first.State != agent.StateRunning {
		t.Fatalf("first: %+v %v", first, err)
	}
	k.turns["turn-1"] = agent.StateDone
	again, err := k.start(t, phone, "review this", "key-1")
	if err != nil || again.TurnID != "turn-1" || again.State != agent.StateDone || k.rec.runs != 1 {
		t.Fatalf("repeat: %+v %v, %d runs", again, err, k.rec.runs)
	}
}

// The same key with a different request is refused, never answered with
// the first one's turn.
func TestAKeyReusedForAnotherRequestIsRefused(t *testing.T) {
	k := keyedRouter(t)
	if _, err := k.start(t, phone, "review this", "key-1"); err != nil {
		t.Fatal(err)
	}
	k.turns["turn-1"] = agent.StateRunning
	if _, err := k.start(t, phone, "something else", "key-1"); !errors.Is(err, rpc.ErrInvalidParams) {
		t.Fatalf("a different request: %v, want invalid params", err)
	}
}

// A replayed start whose turn the manager forgot answers ended when the
// turn opened a chat, and runs anew when it never did.
func TestAReplayedStartAfterItsTurnIsForgotten(t *testing.T) {
	k := keyedRouter(t)
	if _, err := k.start(t, phone, "review this", "key-1"); err != nil {
		t.Fatal(err)
	}
	if err := k.db.SetTurnChat("turn-1", "codex:abc", 1); err != nil {
		t.Fatal(err)
	}
	opened, err := k.start(t, phone, "review this", "key-1")
	if err != nil || opened.TurnID != "turn-1" || opened.State != agent.StateDone || k.rec.runs != 1 {
		t.Fatalf("with a chat: %+v %v, %d runs", opened, err, k.rec.runs)
	}

	if _, err := k.start(t, phone, "and this", "key-2"); err != nil {
		t.Fatal(err)
	}
	again, err := k.start(t, phone, "and this", "key-2")
	if err != nil || again.TurnID != "turn-3" || k.rec.runs != 3 {
		t.Fatalf("with no chat: %+v %v, %d runs", again, err, k.rec.runs)
	}
}

// An agent's key is ignored: agents don't resend, and one reusing "1"
// must not have its next message dropped.
func TestAnAgentsKeyIsIgnored(t *testing.T) {
	k := keyedRouter(t)
	agentCaller := rpc.Caller{Device: "phone", Scope: rpc.ScopeLocal, Agent: &rpc.AgentCaller{ChatID: "claude:parent"}}
	for range 2 {
		if _, err := k.start(t, agentCaller, "continue", "1"); err != nil {
			t.Fatal(err)
		}
	}
	if k.rec.runs != 2 {
		t.Fatalf("%d runs, want 2", k.rec.runs)
	}
}

// Agents aren't told about the key at all.
func TestAgentsToolSchemasLeaveOutTheKey(t *testing.T) {
	k := keyedRouter(t)
	for _, def := range k.r.ToolDefs() {
		props, _ := def.InputSchema["properties"].(map[string]any)
		if _, ok := props["idempotency_key"]; ok {
			t.Errorf("%s offers agents idempotency_key", def.Name)
		}
	}
}
