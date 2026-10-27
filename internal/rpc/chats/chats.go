// Package chats is the `chats.*` methods: the transcript surface a client
// pages, sends into, and subscribes to; the workflows live in internal/chat.
package chats

import (
	"context"
	"time"

	"github.com/repogoapp/host/internal/chatwire"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/chat"
	"github.com/repogoapp/host/internal/chatlive"
	"github.com/repogoapp/host/internal/handoff"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/store"
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
	rpc.Add(r, "chats.list", d.list, rpc.Tool("List the chats on this environment, newest activity first, "+
		"filtered and sorted like the chat drawer. Empty cwd is every project."),
		rpc.Read, rpc.LiveOn("chats.changed", "chats.removed"))
	// info is one chat's row without its transcript, for refreshing a snapshot
	// from chats.list without refetching fifty.
	rpc.Add(r, "chats.info", d.info, rpc.Tool("One chat's row: title, project, status, last reply."),
		rpc.Read, rpc.LiveOn("chats.changed", "chats.removed"))
	// sync keeps the phone's copy of every chat row current from a cursor.
	rpc.Add(r, "chats.sync", d.sync)
	rpc.Add(r, "chats.messages", d.messages, rpc.Tool("A page of one chat's messages. "+
		"tail with a limit reads the newest; before_index pages older."),
		rpc.Read, rpc.LiveOn("chats.changed"))
	// Every prompt's index, so the scrubber reaches prompts not yet paged in.
	rpc.Add(r, "chats.subagent", d.subagent)
	rpc.Add(r, "chats.workflow", d.workflow)
	// A tool sheet's calls whole, and each again as its rows land while open.
	rpc.Add(r, "chats.tools_subscribe", d.toolsSubscribe)
	rpc.Add(r, "chats.tools_unsubscribe", d.toolsUnsubscribe)
	rpc.Add(r, "chats.send", d.send, rpc.Tool("Send a message to a chat. It starts a turn, or queues "+
		"behind the running one; steer adds it to the running turn where the agent allows it."), rpc.Write)
	// Remote-reachable: the caller names a project the host resolves through
	// Contain, not an arbitrary directory.
	rpc.Add(r, "chats.start", d.start, rpc.Detached, rpc.Tool("Start a new chat in a project with "+
		"its first message. Started by an agent, it runs under the calling chat, out of the drawer, "+
		"with no broader permissions; child false opens a normal chat for the user instead."), rpc.Write)
	rpc.Add(r, "chats.wait", d.wait, rpc.Tool("Wait for a turn chats.start or chats.send returned to end, "+
		"then return its state, its chat_id and the agent's last message. A timeout leaves it running; wait again."), rpc.Read)
	rpc.Add(r, "chats.resolve", d.resolve, rpc.Tool("Resolve a chat (moves it to the done pile) or unresolve it."), rpc.Write)
	rpc.Add(r, "chats.pin", d.pin, rpc.Tool("Pin a chat to the top of the drawer, or unpin it."), rpc.Write)
	rpc.Add(r, "chats.update", d.update, rpc.Tool("Rename a chat. The title is written to the agent's own "+
		"files, so its CLI shows it too."), rpc.Write)
	rpc.Add(r, "chats.delete", d.delete)
	// stop is Stop on a chat rather than a turn, for a caller that knows the
	// chat is running but not which turn.
	rpc.Add(r, "chats.stop", d.stop, rpc.Tool("Stop a chat's running turn and the turns of the chats its agent started."), rpc.Write)
	rpc.Add(r, "chats.subscribe", d.subscribe)
	rpc.Add(r, "chats.unsubscribe", d.unsubscribe)
	// One chat's queued turns, fetched when its row's queue_revision moves: the row
	// carries the count, so nothing is watched.
	rpc.Add(r, "chats.queue", d.queue)
	// Opens the chat in the agent's own CLI in a terminal window here.
	rpc.Add(r, "chats.handoff", d.handoff)
}

func (d Deps) list(ctx context.Context, _ rpc.Caller, a ListParams) (ListResult, error) {
	q := a.store()
	q.Limit, q.Cursor = a.Limit, a.Cursor
	page, err := d.Chats.List(ctx, q, a.Refresh)
	return ListResult{Chats: page.Chats, NextCursor: page.NextCursor}, err
}

func (d Deps) sync(ctx context.Context, _ rpc.Caller, a store.SyncParams) (store.SyncResult, error) {
	return d.Chats.Sync(ctx, a)
}

func (d Deps) info(_ context.Context, _ rpc.Caller, a ChatParams) (store.Chat, error) {
	return d.Chats.Info(a.ChatID)
}

func (d Deps) messages(_ context.Context, _ rpc.Caller, a MessagesParams) (chatwire.Page, error) {
	page, err := d.Chats.Messages(a.ChatID, chat.Page{SinceIndex: a.SinceIndex, Limit: a.Limit, Tail: a.Tail, BeforeIndex: a.BeforeIndex})
	if err != nil {
		return chatwire.Page{}, err
	}
	return d.Wire.Page(page), nil
}

// subagent is in the same event shape as chats.messages. With agent_id it is
// that agent of the workflow call_id started.
func (d Deps) subagent(_ context.Context, _ rpc.Caller, a SubagentParams) (SubagentResult, error) {
	sub, rows, err := d.Chats.Subagent(a.ChatID, a.CallID, a.AgentID)
	if err != nil {
		return SubagentResult{}, err
	}
	return SubagentResult{
		Kind: sub.Kind, Name: sub.Name,
		Events: d.Wire.Messages(a.ChatID.Kind(), rows), Tools: d.Wire.Details(a.ChatID.Kind(), rows),
	}, nil
}

// workflow is a Workflow call's agents, each as its row on the run's page draws it.
func (d Deps) workflow(_ context.Context, _ rpc.Caller, a WorkflowParams) (WorkflowResult, error) {
	subs, err := d.Chats.Subagents(a.ChatID, a.CallID)
	if err != nil {
		return WorkflowResult{}, err
	}
	agents := make([]WorkflowAgent, 0, len(subs))
	for _, s := range subs {
		agents = append(agents, WorkflowAgent{
			ID: s.Key, Label: s.Label, Phase: s.Phase, Done: s.Done,
			StartedAt: s.StartedAt, UpdatedAt: s.UpdatedAt, Result: s.Result,
		})
	}
	return WorkflowResult{Agents: agents}, nil
}

func (d Deps) toolsSubscribe(_ context.Context, c rpc.Caller, a ToolsSubscribeParams) (ToolsSubscribeResult, error) {
	rows, err := d.Chats.ToolCalls(a.ChatID, a.CallIDs)
	if err != nil {
		return ToolsSubscribeResult{}, err
	}
	d.Live.WatchTools(c.Device, a.ChatID, a.CallIDs)
	return ToolsSubscribeResult{Tools: d.Wire.Details(a.ChatID.Kind(), rows)}, nil
}

func (d Deps) toolsUnsubscribe(_ context.Context, c rpc.Caller, a ToolsUnsubscribeParams) (rpc.Ack, error) {
	d.Live.UnwatchTools(c.Device, a.ChatID, a.CallIDs)
	return rpc.OK, nil
}

func (d Deps) send(_ context.Context, c rpc.Caller, a SendParams) (SendResult, error) {
	a.Turn.Device, a.Turn.From = c.Device, from(c)
	if a.Steer {
		status, steered, err := d.Chats.Steer(a.ChatID, a.Turn)
		return SendResult{TurnID: status.TurnID, State: status.State, Steered: steered}, err
	}
	status, err := d.Chats.Send(a.ChatID, a.Turn)
	return SendResult{TurnID: status.TurnID, State: status.State}, err
}

func (d Deps) start(_ context.Context, c rpc.Caller, a StartParams) (StartResult, error) {
	a.Turn.Device, a.Turn.From = c.Device, from(c)
	where := chat.Start{Path: a.Cwd, Agent: a.Agent, Title: a.Title, ProjectAgent: a.ProjectAgent}
	if c.Agent != nil && (a.Child == nil || *a.Child) {
		where.Parent = c.Agent.ChatID
	}
	status, err := d.Chats.Start(where, a.Turn)
	return StartResult{TurnID: status.TurnID, State: status.State}, err
}

// from is the chat an agent calls from, whose settings cap what it sends;
// nil for a device.
func from(c rpc.Caller) *chat.Caller {
	if c.Agent == nil {
		return nil
	}
	return &chat.Caller{ChatID: c.Agent.ChatID, Config: agent.TurnConfig{
		PermissionMode: agent.PermissionMode(c.Agent.PermissionMode), Mode: c.Agent.Mode,
	}}
}

// The wait's default stays under the 60 s an MCP tool call gets by default.
const (
	waitDefault = 50 * time.Second
	waitMax     = 5 * time.Minute
)

func (d Deps) wait(ctx context.Context, _ rpc.Caller, a WaitParams) (WaitResult, error) {
	timeout := min(time.Duration(a.TimeoutMS)*time.Millisecond, waitMax)
	if timeout <= 0 {
		timeout = waitDefault
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	w, err := d.Chats.Wait(ctx, a.TurnID)
	if err != nil {
		return WaitResult{}, err
	}
	out := WaitResult{TurnID: w.Status.TurnID, State: w.Status.State, Reply: w.Reply, Error: w.Status.Error}
	// Before the agent names its session the id is a placeholder no list knows.
	if _, _, named := agent.SplitChatID(w.Status.ChatID); named {
		out.ChatID = store.ChatID(w.Status.ChatID)
	}
	return out, nil
}

func (d Deps) resolve(_ context.Context, _ rpc.Caller, a ResolveParams) (ResolveResult, error) {
	c, err := d.Chats.Resolve(a.ChatID, a.Resolved)
	return ResolveResult{Chat: c}, err
}

func (d Deps) pin(_ context.Context, _ rpc.Caller, a PinParams) (PinResult, error) {
	c, err := d.Chats.Pin(a.ChatID, a.Pinned)
	return PinResult{Chat: c}, err
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
	return d.Live.Subscribe(c.Device, a.ChatID, a.SinceIndex)
}

func (d Deps) unsubscribe(_ context.Context, c rpc.Caller, _ rpc.None) (rpc.Ack, error) {
	d.Live.Unsubscribe(c.Device)
	return rpc.OK, nil
}

func (d Deps) queue(_ context.Context, _ rpc.Caller, a ChatParams) (store.Queue, error) {
	return d.Chats.Queue(a.ChatID)
}

func (d Deps) handoff(ctx context.Context, _ rpc.Caller, a ChatParams) (rpc.Ack, error) {
	c, err := d.Chats.Info(a.ChatID)
	if err != nil {
		return rpc.Ack{}, err
	}
	p, ok := d.Agents.Lookup(agent.Kind(c.Agent))
	if !ok {
		return rpc.Ack{}, handoff.ErrNoResume
	}
	command, err := handoff.Command(p, c.ID.SessionID(), c.CWD)
	if err != nil {
		return rpc.Ack{}, err
	}
	if err := handoff.Open(ctx, command); err != nil {
		return rpc.Ack{}, err
	}
	return rpc.OK, nil
}
