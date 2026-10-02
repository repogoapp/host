// Package turns is the `limits.*` and `turns.*` methods: what is running, and
// answering what a running agent asked.
package turns

import (
	"context"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agentusage"
	"github.com/repogo/host/internal/rpc"
)

// Deps are the turn manager, which only the host has (it owns the CLI
// process), and the usage reader.
type Deps struct {
	Runner *agent.Manager
	Usage  *agentusage.Service
}

func Register(r *rpc.Router, d Deps) {
	// usage reports each provider's remaining rate limit; read-only, it never
	// costs quota.
	rpc.Add(r, "limits.read", d.usage)
	// reset spends one reset credit (listed in limits.read) and answers Codex's
	// outcome; "nothing_to_reset" means the credit was kept.
	rpc.Add(r, "limits.reset", d.reset, rpc.Detached)
	// Local-only because a caller that picks the cwd picks what the agent may
	// read and write; chats.send is the remote equivalent.
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
	outcome, err := d.Usage.Reset(ctx, agent.Kind(a.Kind), a.CreditID)
	return ResetResult{Outcome: outcome}, err
}

func (d Deps) list(context.Context, rpc.Caller, rpc.None) (TurnsResult, error) {
	return TurnsResult{Turns: d.Runner.List()}, nil
}

func (d Deps) get(_ context.Context, _ rpc.Caller, a TurnParams) (agent.TurnStatus, error) {
	return d.Runner.Status(a.TurnID)
}

func (d Deps) stop(_ context.Context, _ rpc.Caller, a StopParams) (rpc.Ack, error) {
	return rpc.OK, d.Runner.Stop(a.TurnID)
}

func (d Deps) respond(_ context.Context, _ rpc.Caller, a RespondParams) (rpc.Ack, error) {
	return rpc.OK, d.Runner.Respond(a.TurnID, a.CallID, a.Answer)
}

func (d Deps) editQueued(_ context.Context, _ rpc.Caller, a EditQueuedParams) (rpc.Ack, error) {
	return rpc.OK, d.Runner.EditQueued(a.TurnID, a.Prompt)
}

func (d Deps) removeQueued(_ context.Context, _ rpc.Caller, a RemoveQueuedParams) (rpc.Ack, error) {
	return rpc.OK, d.Runner.RemoveQueued(a.TurnID)
}

func (d Deps) sendQueued(_ context.Context, c rpc.Caller, a TurnParams) (agent.TurnStatus, error) {
	return d.Runner.SendQueued(a.TurnID, c.Device)
}
