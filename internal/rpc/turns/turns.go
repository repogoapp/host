// Package turns is the `limits.*` and `turns.*` methods: what is running, and
// answering what a running agent asked.
package turns

import (
	"context"
	"errors"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentusage"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/store"
)

// Deps are the turn manager, which only the host has (it owns the CLI
// process), and the usage reader.
type Deps struct {
	Runner *agent.Manager
	Usage  *agentusage.Service
	// TurnChats is the chat each turn opened, kept a day, for a turn the
	// Runner has forgotten.
	TurnChats interface {
		TurnChat(turnID string) (store.ChatID, error)
	}
}

func Register(r *rpc.Router, d Deps) {
	// usage reports each provider's remaining rate limit; read-only, it never
	// costs quota.
	rpc.Add(r, "limits.read", d.usage, rpc.Tool("Each agent's plan and how much of each rate-limit "+
		"window is used, with when it resets."), rpc.Read)
	// reset spends one reset credit (listed in limits.read) and answers Codex's
	// outcome; "nothing_to_reset" means the credit was kept.
	rpc.Add(r, "limits.reset", d.reset, rpc.Detached)
	rpc.Add(r, "turns.list", d.list)
	rpc.Add(r, "turns.get", d.get)
	// The user's verbs on a turn, in the order they act: stop it, answer it,
	// and change or drop one still queued.
	rpc.Add(r, "turns.stop", d.stop)
	// respond answers a pending approval by (turn, call): a turn can wait on
	// more than one, and "the current one" is ambiguous exactly when it matters.
	rpc.Add(r, "turns.respond", d.respond)
	rpc.Add(r, "turns.edit_queued", d.editQueued)
	rpc.Add(r, "turns.remove_queued", d.removeQueued)
	// Send Now: a queued turn runs at once with what it was queued with, its
	// files included, while its chat runs nothing.
	rpc.Add(r, "turns.send_queued", d.sendQueued)
}

func (d Deps) usage(ctx context.Context, _ rpc.Caller, _ rpc.None) (UsageResult, error) {
	return UsageResult{Agents: d.Usage.All(ctx)}, nil
}

func (d Deps) reset(ctx context.Context, _ rpc.Caller, a ResetParams) (ResetResult, error) {
	outcome, err := d.Usage.Reset(ctx, agent.Kind(a.Agent), a.CreditID)
	return ResetResult{Outcome: outcome}, err
}

func (d Deps) list(context.Context, rpc.Caller, rpc.None) (TurnsResult, error) {
	all := d.Runner.List()
	turns := make([]Turn, len(all))
	for i, s := range all {
		turns[i] = turnOf(s)
	}
	return TurnsResult{Turns: turns}, nil
}

// get answers for an ended turn the Runner forgot too, from the chat it
// opened: a start replayed after its turn ended still finds its chat.
func (d Deps) get(_ context.Context, _ rpc.Caller, a TurnParams) (Turn, error) {
	s, err := d.Runner.Status(a.TurnID)
	if errors.Is(err, agent.ErrNotFound) {
		if chat, _ := d.TurnChats.TurnChat(a.TurnID); chat != "" {
			_, session, _ := agent.SplitChatID(string(chat))
			return Turn{TurnID: a.TurnID, ChatID: string(chat), State: agent.StateDone, SessionID: session}, nil
		}
	}
	return turnOf(s), err
}

// stop stops a running turn, or drops one still queued.
func (d Deps) stop(_ context.Context, _ rpc.Caller, a TurnParams) (rpc.Ack, error) {
	return rpc.OK, d.Runner.Stop(a.TurnID)
}

func (d Deps) respond(_ context.Context, _ rpc.Caller, a RespondParams) (rpc.Ack, error) {
	return rpc.OK, d.Runner.Respond(a.TurnID, a.CallID, a.Answer)
}

func (d Deps) editQueued(_ context.Context, _ rpc.Caller, a EditQueuedParams) (rpc.Ack, error) {
	return rpc.OK, d.Runner.EditQueued(a.TurnID, a.Prompt)
}

// removeQueued drops a turn only while it still waits, so a queue row acted
// on late cannot stop the turn it has become.
func (d Deps) removeQueued(_ context.Context, _ rpc.Caller, a TurnParams) (rpc.Ack, error) {
	return rpc.OK, d.Runner.RemoveQueued(a.TurnID)
}

func (d Deps) sendQueued(_ context.Context, c rpc.Caller, a TurnParams) (Turn, error) {
	s, err := d.Runner.SendQueued(a.TurnID, c.Device)
	return turnOf(s), err
}
