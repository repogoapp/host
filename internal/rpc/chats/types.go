package chats

import (
	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/chat"
	"github.com/repogoapp/host/internal/chatwire"
	"github.com/repogoapp/host/internal/store"
)

type ChatParams struct {
	ChatID store.ChatID `json:"chat_id" doc:"The chat's id, as chats.list gives it"`
}

// Query is one reading of the inbox: the same keys for the list and for a
// chat's neighbours in it, so the arrows step through exactly the rows shown.
type Query struct {
	// Cwd scopes the list to one project; Cwds narrows it to a set. Empty is
	// every chat on this host.
	Cwd  string   `json:"cwd" doc:"A project's folder; empty is every project"`
	Cwds []string `json:"cwds" doc:"Several projects' folders"`

	// Resolved: true is the done pile, false the live list, null both.
	Resolved *bool `json:"resolved" doc:"true: only resolved chats; false: only open ones; omit for both"`

	// Agent narrows to one agent's chats ("claude", "codex"); empty is all.
	Agent string `json:"agent" doc:"Only this agent's chats: claude, codex; empty is all"`

	// Sort: "activity" (default), "created" or "title". Order: "desc" (default) or "asc".
	Sort  string `json:"sort" doc:"activity (default), created or title"`
	Order string `json:"order" doc:"desc (default) or asc"`

	// Search keeps the chats whose title, prompts and replies hold every word
	// of it; each row then carries match, the words around the hit.
	Search string `json:"search" doc:"Words a chat's title, prompts or replies must all hold"`

	// Parent lists the chats one chat's agent started; empty leaves them out
	// unless Search is set, as the drawer does.
	Parent store.ChatID `json:"parent" doc:"Only the chats this chat's agent started; empty leaves those out"`
}

func (q Query) store() store.ChatQuery {
	return store.ChatQuery{
		Cwd: q.Cwd, Cwds: q.Cwds, Resolved: q.Resolved, Agent: q.Agent, Search: q.Search, Parent: q.Parent,
		Sort: store.ChatSort(q.Sort), Order: store.ChatOrder(q.Order),
	}
}

type ListParams struct {
	Query
	Limit   int  `json:"limit" doc:"Chats per page"`
	Refresh bool `json:"refresh" doc:"Re-read the agents' files first; slow, leave false"`

	// Cursor is the previous reply's next_cursor; empty starts over.
	Cursor string `json:"cursor" doc:"The previous page's next_cursor; empty starts over"`
}

type ListResult struct {
	Chats      []store.Chat `json:"chats" wire:"array"`
	NextCursor string       `json:"next_cursor"`
}

type MessagesParams struct {
	ChatID     store.ChatID `json:"chat_id" doc:"The chat's id, as chats.list gives it"`
	SinceIndex int          `json:"since_index" doc:"Messages after this index"`
	Limit      int          `json:"limit" doc:"Messages per page"`

	// A transcript opens at the bottom and pages upward, so the two directions
	// need separate cursors. Absent both, this is the live push cursor's read.
	Tail        bool `json:"tail" doc:"Read the newest messages"`
	BeforeIndex *int `json:"before_index" doc:"Messages before this index, to page older"`
}

type ToolsSubscribeParams struct {
	ChatID  store.ChatID `json:"chat_id"`
	CallIDs []string     `json:"call_ids" wire:"array"`
}

type ToolsSubscribeResult struct {
	Tools []chatwire.ToolDetail `json:"tools" wire:"array"`
}

type ToolsUnsubscribeParams struct {
	ChatID  store.ChatID `json:"chat_id"`
	CallIDs []string     `json:"call_ids" wire:"array"`
}

type SubagentParams struct {
	ChatID store.ChatID `json:"chat_id"`
	CallID string       `json:"call_id"`
	// AgentID names an agent of the workflow CallID started; such an agent
	// has no call of its own.
	AgentID string `json:"agent_id,omitempty"`
}

type WorkflowParams struct {
	ChatID store.ChatID `json:"chat_id"`
	CallID string       `json:"call_id"`
}

// WorkflowResult is a Workflow call's agents in the order its run started them.
type WorkflowResult struct {
	Agents []WorkflowAgent `json:"agents" wire:"array"`
}

// WorkflowAgent is one agent of a workflow run, as its row draws it.
type WorkflowAgent struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Phase string `json:"phase"`
	// Done is set once the run recorded the agent's result.
	Done bool `json:"done"`
	// StartedAt and UpdatedAt are Unix milliseconds: its transcript's first
	// line, and its last write.
	StartedAt int64 `json:"started_at"`
	UpdatedAt int64 `json:"updated_at"`
	// Result is the agent's answer, cut short for the row.
	Result string `json:"result"`
}

// SubagentResult is a subagent's steps: its type and name for the page's
// header, and its rows in chats.messages' shape.
type SubagentResult struct {
	Kind   string             `json:"kind"`
	Name   string             `json:"name"`
	Events []chatwire.Message `json:"events" wire:"array"`
	// Tools is every step's call whole: a subagent's page is itself a sheet.
	Tools []chatwire.ToolDetail `json:"tools" wire:"array"`
}

type SendParams struct {
	ChatID store.ChatID `json:"chat_id" doc:"The chat's id, as chats.list gives it"`
	Turn   chat.Turn    `json:"turn" doc:"The message"`
	// Steer adds the turn to the chat's running one where the agent allows it.
	Steer bool `json:"steer" doc:"Add it to the running turn instead of queueing it"`
}

type SendResult struct {
	TurnID string          `json:"turn_id"`
	State  agent.TurnState `json:"state"`
	// Steered means the running turn TurnID took it; otherwise it was queued or started.
	Steered bool `json:"steered"`
}

// StartParams starts a chat in the folder at Cwd, as chats.list filters by it. Title is the
// chat's name, written on the phone from the prompt; empty leaves the
// prompt's opening words.
type StartParams struct {
	Turn  chat.Turn `json:"turn" doc:"The first message"`
	Cwd   string    `json:"cwd" doc:"The project's folder"`
	Agent string    `json:"agent" doc:"claude or codex"`
	Title string    `json:"title" doc:"The chat's name; empty takes the message's opening words"`
	// ProjectAgent names one of the project's own agents when Agent is acp.
	ProjectAgent string `json:"project_agent,omitempty"`
	// Child is for an agent starting a chat: unset or true starts one under
	// the calling chat, out of the drawer; false opens one for the user.
	Child *bool `json:"child,omitempty" doc:"For an agent: false opens a normal chat for the user; omit to start one under this chat"`
}

type StartResult struct {
	TurnID string          `json:"turn_id"`
	State  agent.TurnState `json:"state"`
}

// WaitParams names a turn, as chats.start and chats.send return it. TimeoutMS
// is how long to wait; zero is waitDefault, and it is capped at waitMax.
type WaitParams struct {
	TurnID    string `json:"turn_id" doc:"The turn_id chats.start or chats.send returned"`
	TimeoutMS int    `json:"timeout_ms" doc:"How long to wait; default 50000, at most 300000"`
}

// WaitResult is the turn when it ended or the wait ran out. ChatID is set once
// the agent has named its chat; Reply is its last message.
type WaitResult struct {
	TurnID string          `json:"turn_id"`
	ChatID store.ChatID    `json:"chat_id,omitempty"`
	State  agent.TurnState `json:"state"`
	Reply  string          `json:"reply,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type ResolveParams struct {
	ChatID   store.ChatID `json:"chat_id" doc:"The chat's id, as chats.list gives it"`
	Resolved bool         `json:"resolved" doc:"true resolves; false reopens"`
}

type ResolveResult struct {
	Chat store.Chat `json:"chat"`
}

type PinParams struct {
	ChatID store.ChatID `json:"chat_id" doc:"The chat's id, as chats.list gives it"`
	Pinned bool         `json:"pinned" doc:"true pins; false unpins"`
}

type PinResult struct {
	Chat store.Chat `json:"chat"`
}

// UpdateParams changes only the fields it sets; see chat.Update.
type UpdateParams struct {
	ChatID store.ChatID `json:"chat_id" doc:"The chat's id, as chats.list gives it"`
	chat.Update
}

type UpdateResult struct {
	Chat store.Chat `json:"chat"`
}

type SubscribeParams struct {
	ChatID     store.ChatID `json:"chat_id"`
	SinceIndex int          `json:"since_index"`
}
