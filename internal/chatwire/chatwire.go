// Package chatwire is a chat's transcript as the phone receives it. The store
// keeps rows as parsed; this builds what leaves the host on every send, so a
// change here never needs a resync. A tool row carries what the chat draws
// (its name, state, icon and labels, and the few fields the chat reads
// without opening the call), never the call's input or output.
package chatwire

import (
	"encoding/json"
	"path"
	"strconv"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/toollabel"
)

// Page is store.Page with its rows built for the phone: the result of
// chats.messages and the body of chats.appended.
type Page struct {
	ChatID store.ChatID `json:"chat_id"`
	Events []Message    `json:"events" wire:"array"`

	// Where the conversation is happening. HostID is stamped by the transport.
	HostID string `json:"host_id,omitempty"`
	Cwd    string `json:"cwd"`
	Agent  string `json:"agent"`

	// Generation the rows came from; a client holding another refetches from 0.
	Generation int64 `json:"generation"`

	// EventCount is the transcript's total; NextIdx is the next `since_idx`,
	// FirstIdx the next `before_idx`, and HasBefore false once index 0 is here.
	EventCount int  `json:"event_count"`
	NextIdx    int  `json:"next_idx"`
	FirstIdx   int  `json:"first_idx"`
	HasBefore  bool `json:"has_before"`
}

// Message is one transcript row.
type Message struct {
	Idx  int    `json:"idx"`
	Kind string `json:"kind"`
	Turn string `json:"turn,omitempty"`
	Text string `json:"text,omitempty"`
	Tool *Tool  `json:"tool,omitempty"`

	// Unix milliseconds; zero when the transcript line carried none.
	At int64 `json:"at,omitempty"`
}

// Tool is one tool call on the chat lane. A call and its result are two
// rows with the same CallID; the phone merges them.
type Tool struct {
	CallID string `json:"call_id,omitempty"`

	// Name is the tool the call ran, as the chat should read it: a Codex code
	// cell that calls one tool is named for that tool. Empty on a result row
	// that doesn't repeat it.
	Name  string `json:"name,omitempty"`
	State State  `json:"state"`

	// Calls is every tool a Codex code cell that calls several ran, in order,
	// so the chat's summary counts each one instead of one "exec".
	Calls []string `json:"calls,omitempty"`

	// Icon and Labels are set on a row that names its tool; a result row that
	// doesn't keeps the call row's.
	Icon   string           `json:"icon,omitempty"`
	Labels *toollabel.Lines `json:"labels,omitempty"`

	// Todos is the list a todo tool wrote (Claude's TodoWrite, Codex's
	// update_plan), for the composer's todo strip.
	Todos []Todo `json:"todos,omitempty"`

	// Questions is a non-blocking question's form as the agent wrote it
	// (Codex's request_user_input_async). Blocking questions and approvals
	// come on chats.approval instead.
	Questions json.RawMessage `json:"questions,omitempty"`

	// RecordingID is the browser recording a browser call reported.
	RecordingID string `json:"recording_id,omitempty"`

	// SubagentType is a subagent's type ("Explore"), drawn after its label;
	// Background is set once a subagent was launched to run on its own.
	SubagentType string `json:"subagent_type,omitempty"`
	Background   bool   `json:"background"`

	// fileName is the name of the file the call worked on, for voice progress.
	File string `json:"file,omitempty"`
}

// State is where a tool call is: running until its output lands, then
// completed or failed.
type State string

const (
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
)

// Todo is one item of a todo list. Status is as the agent wrote it
// ("in_progress", "completed", …); absent reads as pending.
type Todo struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status,omitempty"`
}

// Labeler is one agent's side of the chat lane. ToolRow fills a row's Name,
// Icon, Labels and the agent's own fields (the builder adds the rest); Resolve
// is the call as a sheet shows it, a one-tool Codex cell as that tool.
type Labeler interface {
	Kind() agent.Kind
	ToolRow(call agent.ToolCall) Tool
	Resolve(call agent.ToolCall) agent.ToolCall
}

// ToolDetail is one tool call whole, for a sheet that shows it: the reply to
// chats.tools_subscribe and the body of chats.tool.
type ToolDetail struct {
	CallID string          `json:"call_id"`
	Name   string          `json:"name"`
	State  State           `json:"state"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`

	// OutputBytes is the output's full size when it was cut to MaxOutput.
	OutputBytes int `json:"output_bytes,omitempty"`

	// Result is what the agent recorded beyond the output: exit code, time,
	// stderr, a cell's commands, a search's links. Only the sheet gets it.
	Result *agent.ToolResult `json:"result,omitempty"`
}

// MaxOutput is how much of a call's output a sheet gets; a command can print
// megabytes, and the first part is what a phone reads.
const MaxOutput = 256 << 10

// Builder turns stored rows into the chat lane, labelling each tool call by
// the agent the chat belongs to.
type Builder struct {
	labelers map[agent.Kind]Labeler
}

func New(labelers []Labeler) *Builder {
	b := &Builder{labelers: map[agent.Kind]Labeler{}}
	for _, l := range labelers {
		b.labelers[l.Kind()] = l
	}
	return b
}

// Page is p with its rows built for the phone.
func (b *Builder) Page(p store.Page) Page {
	return Page{
		ChatID: p.ChatID, Events: b.Messages(agent.Kind(p.Agent), p.Events),
		HostID: p.HostID, Cwd: p.Cwd, Agent: p.Agent, Generation: p.Generation,
		EventCount: p.EventCount, NextIdx: p.NextIdx, FirstIdx: p.FirstIdx, HasBefore: p.HasBefore,
	}
}

// Messages builds the rows of a chat by agent kind; a subagent's rows are
// labelled by the chat that started it.
func (b *Builder) Messages(kind agent.Kind, rows []store.Message) []Message {
	out := make([]Message, len(rows))
	for i, m := range rows {
		out[i] = Message{Idx: m.Idx, Kind: m.Kind, Turn: m.Turn, Text: m.Text, At: m.At}
		if m.Tool == "" {
			continue
		}
		var call agent.ToolCall
		if json.Unmarshal([]byte(m.Tool), &call) != nil {
			continue
		}
		tool := b.row(kind, call)
		tool.CallID = call.CallID
		tool.State = state(agent.EventKind(m.Kind), call)
		tool.RecordingID = recordingID(call.Output)
		out[i].Tool = &tool
	}
	return out
}

// row asks the chat's agent to name the call; an agent this host no longer
// registers still gets the shared vocabulary.
func (b *Builder) row(kind agent.Kind, call agent.ToolCall) Tool {
	if l, ok := b.labelers[kind]; ok {
		return l.ToolRow(call)
	}
	if call.Name == "" {
		return Tool{}
	}
	args, _ := toollabel.Input(call.Input)
	return Labeled(call.Name, toollabel.Shared(call.Name, args), args)
}

// Details merges one chat's call and result rows (ToolCalls) into a call each,
// in the order they were first seen.
func (b *Builder) Details(kind agent.Kind, rows []store.Message) []ToolDetail {
	var order []string
	calls := map[string]*agent.ToolCall{}
	finished := map[string]bool{}
	for _, m := range rows {
		var row agent.ToolCall
		if json.Unmarshal([]byte(m.Tool), &row) != nil || row.CallID == "" {
			continue
		}
		call, ok := calls[row.CallID]
		if !ok {
			order = append(order, row.CallID)
			call = &agent.ToolCall{CallID: row.CallID}
			calls[row.CallID] = call
		}
		if call.Name == "" {
			call.Name = row.Name
		}
		if len(call.Input) == 0 {
			call.Input = row.Input
		}
		if row.Output != "" {
			call.Output = row.Output
		}
		if row.Result != nil {
			call.Result = row.Result
		}
		call.IsError = call.IsError || row.IsError
		finished[row.CallID] = finished[row.CallID] || agent.EventKind(m.Kind) == agent.EventToolResult
	}
	out := make([]ToolDetail, 0, len(order))
	for _, id := range order {
		out = append(out, b.detail(kind, *calls[id], finished[id]))
	}
	return out
}

func (b *Builder) detail(kind agent.Kind, call agent.ToolCall, finished bool) ToolDetail {
	state := StateRunning
	if finished || call.Output != "" {
		state = StateCompleted
		if call.IsError {
			state = StateFailed
		}
	}
	if l, ok := b.labelers[kind]; ok {
		call = l.Resolve(call)
	}
	d := ToolDetail{CallID: call.CallID, Name: call.Name, State: state, Input: call.Input, Output: call.Output, Result: capped(call.Result)}
	if len(d.Output) > MaxOutput {
		d.OutputBytes = len(d.Output)
		d.Output = cut(d.Output)
	}
	return d
}

// capped is r with its stderr and each command's output cut to MaxOutput,
// copied so the stored call is left whole.
func capped(r *agent.ToolResult) *agent.ToolResult {
	if r == nil {
		return nil
	}
	out := *r
	out.Stderr = cut(out.Stderr)
	out.Commands = make([]agent.ToolCommand, len(r.Commands))
	for i, c := range r.Commands {
		c.Output = cut(c.Output)
		out.Commands[i] = c
	}
	if len(out.Commands) == 0 {
		out.Commands = nil
	}
	return &out
}

// cut is s at most MaxOutput bytes, a rune split at the end dropped.
func cut(s string) string {
	if len(s) <= MaxOutput {
		return s
	}
	return strings.ToValidUTF8(s[:MaxOutput], "")
}

// Labeled is the row for a named call: its name and label, and the file it
// worked on.
func Labeled(name string, label toollabel.Label, args map[string]any) Tool {
	lines := label.Labels
	return Tool{Name: name, Icon: label.Icon, Labels: &lines, File: fileName(args)}
}

// state reads a call row as running until it carries output, and a result
// row as done.
func state(kind agent.EventKind, call agent.ToolCall) State {
	if kind == agent.EventToolCall && call.Output == "" {
		return StateRunning
	}
	if call.IsError {
		return StateFailed
	}
	return StateCompleted
}

// recordingID is the recording a browser call's output names; only RepoGo's
// browser reports one. A Codex code cell prints the tool's MCP result after
// its own lines, so the result is read from the first `{`, `content` and all.
func recordingID(output string) string {
	if !strings.Contains(output, "recordingId") {
		return ""
	}
	if i := strings.Index(output, "{"); i > 0 {
		output = output[i:]
	}
	var result struct {
		RecordingID string `json:"recordingId"`
		Content     []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal([]byte(output), &result) != nil {
		return ""
	}
	if result.RecordingID != "" {
		return result.RecordingID
	}
	for _, part := range result.Content {
		if id := recordingID(part.Text); id != "" {
			return id
		}
	}
	return ""
}

// fileName is the name of the file a call's input points at, under any of the
// keys agents use for it.
func fileName(args map[string]any) string {
	for _, key := range []string{"file_path", "filePath", "path", "filename", "file"} {
		if p := strings.TrimSpace(toollabel.String(args, key)); p != "" {
			return path.Base(p)
		}
	}
	return ""
}

// Todos reads a todo list from a call's output, else its input: `todos`,
// `items` or `plan` (Codex's update_plan), each item's text under the first
// key it has.
func Todos(output string, args map[string]any) []Todo {
	var fromOutput map[string]any
	if json.Unmarshal([]byte(output), &fromOutput) == nil {
		if todos := todoList(fromOutput); len(todos) > 0 {
			return todos
		}
	}
	return todoList(args)
}

func todoList(container map[string]any) []Todo {
	var items []any
	for _, key := range []string{"todos", "items", "plan"} {
		if list, ok := container[key].([]any); ok {
			items = list
			break
		}
	}
	var out []Todo
	for i, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		content := ""
		for _, key := range []string{"content", "text", "title", "label", "step"} {
			if content = strings.TrimSpace(toollabel.String(fields, key)); content != "" {
				break
			}
		}
		if content == "" {
			content = "Todo " + strconv.Itoa(i+1)
		}
		id := strings.TrimSpace(toollabel.String(fields, "id"))
		if id == "" {
			id = strconv.Itoa(i) + "-" + content
		}
		out = append(out, Todo{ID: id, Content: content, Status: toollabel.String(fields, "status")})
	}
	return out
}
