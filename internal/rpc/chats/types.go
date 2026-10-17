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

// HandoffResult is the command the terminal window was opened with.
type HandoffResult struct {
	Command string `json:"command"`
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
}

func (q Query) store() store.ChatQuery {
	return store.ChatQuery{
		Cwd: q.Cwd, Cwds: q.Cwds, Resolved: q.Resolved, Agent: q.Agent, Search: q.Search,
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
	HostID     string       `json:"host_id"`
}

type NeighborsParams struct {
	Query
	ChatID store.ChatID `json:"chat_id"`
}

type NeighborsResult struct {
	Previous *store.Chat `json:"previous"`
	Next     *store.Chat `json:"next"`
	HostID   string      `json:"host_id"`
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
	ChatID store.ChatID          `json:"chat_id"`
	Tools  []chatwire.ToolDetail `json:"tools" wire:"array"`
}

type ToolsUnsubscribeParams struct {
	ChatID  store.ChatID `json:"chat_id"`
	CallIDs []string     `json:"call_ids" wire:"array"`
}

type SubagentParams struct {
	ChatID store.ChatID `json:"chat_id"`
	CallID string       `json:"call_id"`
}

type SubagentResult struct {
	ChatID store.ChatID       `json:"chat_id"`
	CallID string             `json:"call_id"`
	Kind   string             `json:"kind"`
	Name   string             `json:"name"`
	Events []chatwire.Message `json:"events" wire:"array"`
	// Tools is every step's call whole: a subagent's page is itself a sheet.
	Tools  []chatwire.ToolDetail `json:"tools" wire:"array"`
	HostID string                `json:"host_id"`
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
}

type StartResult struct {
	TurnID string          `json:"turn_id"`
	State  agent.TurnState `json:"state"`
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
