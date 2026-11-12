package turns_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/rpc/turns"
	"github.com/repogoapp/host/internal/store"
)

type turnChats map[string]store.ChatID

func (t turnChats) TurnChat(id string) (store.ChatID, error) { return t[id], nil }

// A turn the manager has forgotten still answers from the chat it opened,
// so a start replayed after its turn ended finds its chat; one that opened
// none is not found.
func TestGetAnswersAForgottenTurnFromItsChat(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	mgr := agent.NewManager(log, agent.Hooks{
		Session: func(agent.TurnStatus) {}, Status: func(agent.TurnStatus, agent.ChatStatus, time.Time) {},
		SaveQueue: func(string, []agent.StoredTurn) error { return nil },
	})
	r := rpc.New(log)
	turns.Register(r, turns.Deps{Runner: mgr, TurnChats: turnChats{"turn-1": "codex:abc"}})
	get := func(id string) (turns.Turn, error) {
		b, _ := json.Marshal(map[string]string{"turn_id": id})
		raw, err := r.Call(context.Background(), rpc.Caller{Device: "phone", Scope: rpc.ScopeRemote}, "turns.get", b)
		var out turns.Turn
		if err == nil {
			err = json.Unmarshal(raw, &out)
		}
		return out, err
	}
	got, err := get("turn-1")
	if err != nil || got.ChatID != "codex:abc" || got.SessionID != "abc" || got.State != agent.StateDone {
		t.Fatalf("a forgotten turn with a chat: %+v %v", got, err)
	}
	if _, err := get("turn-2"); !errors.Is(err, rpc.ErrNotFound) {
		t.Fatalf("a turn with no chat: %v, want not found", err)
	}
}
