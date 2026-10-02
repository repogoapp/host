package chats

import (
	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/chat"
	"github.com/repogo/host/internal/chatwire"
	"github.com/repogo/host/internal/store"
)

type ChatParams struct {
	ChatID store.ChatID `json:"chat_id"`
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
	Cwd  string   `json:"cwd"`
	Cwds []string `json:"cwds"`

	// Resolved: true is the done pile, false the live list, null both.
	Resolved *bool `json:"resolved"`

	// Agent narrows to one agent's chats ("claude", "codex", "cursor"); empty is all.
	Agent string `json:"agent"`

	// Sort: "updated" (default), "created" or "title". Order: "desc" (default) or "asc".
	Sort  string `json:"sort"`
	Order string `json:"order"`

	// Search keeps the chats whose title, prompts and replies hold every word
	// of it; each row then carries match, the words around the hit.
	Search string `json:"search"`
}

func (q Query) store() store.ChatQuery {
	return store.ChatQuery{
		Cwd: q.Cwd, Cwds: q.Cwds, Resolved: q.Resolved, Agent: q.Agent, Search: q.Search,
		Sort: store.ChatSort(q.Sort), Order: store.ChatOrder(q.Order),
	}
}

type ListParams struct {
	Query
	Limit   int  `json:"limit"`
	Refresh bool `json:"refresh"`

	// Cursor is the previous reply's next_cursor; empty starts over.
	Cursor string `json:"cursor"`
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
	ChatID   store.ChatID `json:"chat_id"`
	SinceIdx int          `json:"since_idx"`
	Limit    int          `json:"limit"`

	// A transcript opens at the bottom and pages upward, so the two directions
	// need separate cursors. Absent both, this is the live push cursor's read.
	Tail      bool `json:"tail"`
	BeforeIdx *int `json:"before_idx"`
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
	ChatID store.ChatID `json:"chat_id"`
	Turn   chat.Turn    `json:"turn"`
	// Steer adds the turn to the chat's running one where the agent allows it.
	Steer bool `json:"steer"`
}

type SendResult struct {
	TurnID string          `json:"turn_id"`
	State  agent.TurnState `json:"state"`
	// Steered means the running turn TurnID took it; otherwise it was queued or started.
	Steered bool `json:"steered"`
}

// StartParams starts a chat in the project folder at Path. Title is the
// chat's name, written on the phone from the prompt; empty leaves the
// prompt's opening words.
type StartParams struct {
	Turn  chat.Turn `json:"turn"`
	Path  string    `json:"path"`
	Agent string    `json:"agent"`
	Title string    `json:"title"`
}

type StartResult struct {
	TurnID string          `json:"turn_id"`
	State  agent.TurnState `json:"state"`
}

type ResolveParams struct {
	ChatID   store.ChatID `json:"chat_id"`
	Resolved bool         `json:"resolved"`
}

type ResolveResult struct {
	Chat store.Chat `json:"chat"`
}

// UpdateParams changes only the fields it sets; see chat.Update.
type UpdateParams struct {
	ChatID store.ChatID `json:"chat_id"`
	chat.Update
}

type UpdateResult struct {
	Chat store.Chat `json:"chat"`
}

type SubscribeParams struct {
	ChatID   store.ChatID `json:"chat_id"`
	SinceIdx int          `json:"since_idx"`
}
