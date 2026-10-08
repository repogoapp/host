// Package chats is the `chats.*` methods: the transcript surface a client
// pages, sends into, and subscribes to; the workflows live in internal/chat.
package chats

import (
	"context"

	"github.com/repogo/host/internal/chatwire"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/chat"
	"github.com/repogo/host/internal/chatlive"
	"github.com/repogo/host/internal/handoff"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/store"
)

type Deps struct {
	Chats *chat.Service
	// Live pushes a chat's new messages to a device that asked for them.
	Live *chatlive.Manager
	// Agents knows each CLI's resume command, for handoff.
	Agents agent.Providers
	// Wire builds transcript rows as the phone receives them.
	Wire *chatwire.Builder
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "chats.list", d.list)
	// info is one chat's row without its transcript, for refreshing a snapshot
	// from chats.list without refetching fifty.
	rpc.Add(r, "chats.info", d.info)
	// sync keeps the phone's copy of every chat row current from a cursor.
	rpc.Add(r, "chats.sync", d.sync)
	rpc.Add(r, "chats.neighbors", d.neighbors)
	rpc.Add(r, "chats.messages", d.messages)
	rpc.Add(r, "chats.subagent", d.subagent)
	// A tool sheet's calls whole, and each again as its rows land while open.
	rpc.Add(r, "chats.tools_subscribe", d.toolsSubscribe)
	rpc.Add(r, "chats.tools_unsubscribe", d.toolsUnsubscribe)
	rpc.Add(r, "chats.send", d.send)
	// Remote-reachable: the caller names a project the host resolves through
	// Contain, not an arbitrary directory.
	rpc.Add(r, "chats.start", d.start, rpc.Detached)
	rpc.Add(r, "chats.resolve", d.resolve)
	rpc.Add(r, "chats.update", d.update)
	rpc.Add(r, "chats.delete", d.delete)
	// stop is Stop on a chat rather than a turn, for a caller that knows the
	// chat is running but not which turn.
	rpc.Add(r, "chats.stop", d.stop)
	rpc.Add(r, "chats.subscribe", d.subscribe)
	rpc.Add(r, "chats.unsubscribe", d.unsubscribe)
	// One chat's queued turns, fetched when its row's queue_rev moves: the row
	// carries the count, so nothing is watched.
	rpc.Add(r, "chats.queue", d.queue)
	// Opens the chat in the agent's own CLI in a terminal window here.
	rpc.Add(r, "chats.handoff", d.handoff)
}

func (d Deps) list(ctx context.Context, _ rpc.Caller, a ListParams) (ListResult, error) {
	q := a.store()
	q.Limit, q.Cursor = a.Limit, a.Cursor
	page, err := d.Chats.List(ctx, q, a.Refresh)
	return ListResult{Chats: page.Chats, NextCursor: page.NextCursor, HostID: string(d.Chats.Self())}, err
}

func (d Deps) sync(ctx context.Context, _ rpc.Caller, a store.SyncParams) (store.SyncResult, error) {
	return d.Chats.Sync(ctx, a)
}

func (d Deps) info(_ context.Context, _ rpc.Caller, a ChatParams) (store.Chat, error) {
	return d.Chats.Info(a.ChatID)
}

func (d Deps) neighbors(_ context.Context, _ rpc.Caller, a NeighborsParams) (NeighborsResult, error) {
	previous, next, err := d.Chats.Neighbors(a.ChatID, a.store())
	return NeighborsResult{Previous: previous, Next: next, HostID: string(d.Chats.Self())}, err
}

func (d Deps) messages(_ context.Context, _ rpc.Caller, a MessagesParams) (chatwire.Page, error) {
	page, err := d.Chats.Messages(a.ChatID, chat.Page{SinceIdx: a.SinceIdx, Limit: a.Limit, Tail: a.Tail, BeforeIdx: a.BeforeIdx})
	if err != nil {
		return chatwire.Page{}, err
	}
	return d.Wire.Page(page), nil
}

// subagent is in the same event shape as chats.messages.
func (d Deps) subagent(_ context.Context, _ rpc.Caller, a SubagentParams) (SubagentResult, error) {
	sub, err := d.Chats.Subagent(a.ChatID, a.CallID)
	if err != nil {
		return SubagentResult{}, err
	}
	rows := store.MessagesOf(sub.Events)
	return SubagentResult{
		ChatID: a.ChatID, CallID: a.CallID, Kind: sub.Kind, Name: sub.Name,
		Events: d.Wire.Messages(a.ChatID.Kind(), rows), Tools: d.Wire.Details(a.ChatID.Kind(), rows),
		HostID: string(d.Chats.Self()),
	}, nil
}

func (d Deps) toolsSubscribe(_ context.Context, c rpc.Caller, a ToolsSubscribeParams) (ToolsSubscribeResult, error) {
	rows, err := d.Chats.ToolCalls(a.ChatID, a.CallIDs)
	if err != nil {
		return ToolsSubscribeResult{}, err
	}
	d.Live.WatchTools(c.Device, a.ChatID, a.CallIDs)
	return ToolsSubscribeResult{ChatID: a.ChatID, Tools: d.Wire.Details(a.ChatID.Kind(), rows)}, nil
}

func (d Deps) toolsUnsubscribe(_ context.Context, c rpc.Caller, a ToolsUnsubscribeParams) (rpc.Ack, error) {
	d.Live.UnwatchTools(c.Device, a.ChatID, a.CallIDs)
	return rpc.OK, nil
}

func (d Deps) send(_ context.Context, c rpc.Caller, a SendParams) (SendResult, error) {
	a.Turn.Device = c.Device
	if a.Steer {
		status, steered, err := d.Chats.Steer(a.ChatID, a.Turn)
		return SendResult{TurnID: status.TurnID, State: status.State, Steered: steered}, err
	}
	status, err := d.Chats.Send(a.ChatID, a.Turn)
	return SendResult{TurnID: status.TurnID, State: status.State}, err
}

func (d Deps) start(_ context.Context, c rpc.Caller, a StartParams) (StartResult, error) {
	a.Turn.Device = c.Device
	status, err := d.Chats.Start(chat.Start{Path: a.Path, Agent: a.Agent, Title: a.Title}, a.Turn)
	return StartResult{TurnID: status.TurnID, State: status.State}, err
}

func (d Deps) resolve(_ context.Context, _ rpc.Caller, a ResolveParams) (ResolveResult, error) {
	c, err := d.Chats.Resolve(a.ChatID, a.Resolved)
	return ResolveResult{Chat: c}, err
}

func (d Deps) update(_ context.Context, _ rpc.Caller, a UpdateParams) (UpdateResult, error) {
	c, err := d.Chats.Update(a.ChatID, a.Update)
	return UpdateResult{Chat: c}, err
}

func (d Deps) delete(_ context.Context, _ rpc.Caller, a ChatParams) (rpc.Ack, error) {
	return rpc.OK, d.Chats.Delete(a.ChatID)
}

func (d Deps) stop(_ context.Context, _ rpc.Caller, a ChatParams) (rpc.Ack, error) {
	return rpc.OK, d.Chats.Stop(a.ChatID)
}

func (d Deps) subscribe(_ context.Context, c rpc.Caller, a SubscribeParams) (chatlive.Live, error) {
	return d.Live.Subscribe(c.Device, a.ChatID, a.SinceIdx)
}

func (d Deps) unsubscribe(_ context.Context, c rpc.Caller, _ rpc.None) (rpc.Ack, error) {
	d.Live.Unsubscribe(c.Device)
	return rpc.OK, nil
}

func (d Deps) queue(_ context.Context, _ rpc.Caller, a ChatParams) (store.Queue, error) {
	return d.Chats.Queue(a.ChatID)
}

func (d Deps) handoff(ctx context.Context, _ rpc.Caller, a ChatParams) (HandoffResult, error) {
	c, err := d.Chats.Info(a.ChatID)
	if err != nil {
		return HandoffResult{}, err
	}
	p, ok := d.Agents.Lookup(agent.Kind(c.Agent))
	if !ok {
		return HandoffResult{}, handoff.ErrNoResume
	}
	command, err := handoff.Command(p, c.ID.SessionID(), c.CWD)
	if err != nil {
		return HandoffResult{}, err
	}
	return HandoffResult{Command: command}, handoff.Open(ctx, command)
}
