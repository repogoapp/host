package acp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/repogoapp/host/internal/agent"
)

// turnState is one prompt's updates on their way to the phone and the
// transcript. Text streams live as deltas and is written once per message;
// updates and permission requests arrive on different goroutines.
type turnState struct {
	io   agent.TurnIO
	file *transcript

	mu        sync.Mutex
	text      strings.Builder
	reasoning strings.Builder
	tools     map[string]*toolState
	// order is the tool calls by when they were first seen.
	order []string
	usage agent.Usage
}

// toolState is what the agent has said about one tool call so far: ACP sends
// a call, then updates carrying only what changed.
type toolState struct {
	call      agent.ToolCall
	title     string
	name      string
	kind      string
	status    string
	announced bool
	done      bool
}

func (t *turnState) update(u update) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch u.SessionUpdate {
	case updateMessage:
		if text := blockText(u.Content); text != "" {
			t.flushReasoning()
			t.text.WriteString(text)
			t.io.Emit(agent.Event{Kind: agent.EventText, Text: text})
		}
	case updateThought:
		if text := blockText(u.Content); text != "" {
			t.flushText()
			t.reasoning.WriteString(text)
			t.io.Emit(agent.Event{Kind: agent.EventReasoning, Text: text})
		}
	case updateToolCall, updateTool:
		t.tool(u)
	case updateUsage:
		t.usage.ContextUsed, t.usage.ContextSize = u.Used, u.Size
		if u.Cost != nil && strings.EqualFold(u.Cost.Currency, "USD") {
			t.usage.CostUSD = u.Cost.Amount
		}
	}
}

// tool folds an update into its call. The call shows once its input is
// known or it starts, and its result once it ends.
func (t *turnState) tool(u update) {
	st := t.tools[u.ToolCallID]
	if st == nil {
		st = &toolState{call: agent.ToolCall{CallID: u.ToolCallID}, status: statusPending}
		t.tools[u.ToolCallID] = st
		t.order = append(t.order, u.ToolCallID)
	}
	set(&st.title, u.Title)
	set(&st.name, u.Name)
	set(&st.kind, u.Kind)
	set(&st.status, u.Status)
	if present(u.RawInput) {
		st.call.Input = u.RawInput
	}
	st.call.Name = st.label()
	finished := st.status == statusCompleted || st.status == statusFailed
	if !st.announced && (len(st.call.Input) > 0 || st.status != statusPending || finished) {
		t.flushText()
		t.flushReasoning()
		st.announced = true
		t.record(agent.Event{Kind: agent.EventToolCall, Tool: &agent.ToolCall{CallID: st.call.CallID, Name: st.call.Name, Input: st.call.Input}})
	}
	if finished && !st.done {
		st.done = true
		t.record(agent.Event{Kind: agent.EventToolResult, Tool: &agent.ToolCall{
			CallID: st.call.CallID, Name: st.call.Name, Output: toolOutput(u), IsError: st.status == statusFailed,
		}})
	}
}

// set takes a field an update changed; absent or empty leaves it.
func set(field *string, value *string) {
	if value != nil && *value != "" {
		*field = *value
	}
}

// label is what the phone calls a tool: the agent's title for this call,
// else its tool name, else its kind.
func (st *toolState) label() string {
	for _, s := range []string{st.title, st.name, st.kind} {
		if s != "" {
			return s
		}
	}
	return "tool"
}

// permission asks the phone whether a tool may run, with the agent's own
// options; false when the turn ended first or the answer isn't one of them.
func (t *turnState) permission(ctx context.Context, p permissionParams) (string, bool) {
	t.mu.Lock()
	t.tool(p.ToolCall)
	st := t.tools[p.ToolCall.ToolCallID]
	approval := agent.Approval{CallID: p.ToolCall.ToolCallID, Title: st.label(), Kind: st.kind, Input: st.call.Input}
	t.mu.Unlock()
	if approval.Kind == "" {
		approval.Kind = "other"
	}
	for _, o := range p.Options {
		approval.Options = append(approval.Options, agent.ApprovalOption{OptionID: o.OptionID, Name: o.Name, Kind: o.Kind})
	}
	if t.io.Ask == nil {
		return "", false
	}
	answer, err := t.io.Ask(ctx, approval)
	if err != nil {
		return "", false
	}
	var picked string
	if json.Unmarshal(answer, &picked) != nil {
		return "", false
	}
	ok := slices.ContainsFunc(p.Options, func(o permissionOption) bool { return o.OptionID == picked })
	return picked, ok
}

// waitingTool is the newest tool call shown and not yet finished; "" when
// none is.
func (t *turnState) waitingTool() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var newest string
	for _, id := range t.order {
		if st := t.tools[id]; st.announced && !st.done {
			newest = id
		}
	}
	return newest
}

// record shows an event live and keeps it in the transcript.
func (t *turnState) record(e agent.Event) {
	t.io.Emit(e)
	t.write(e)
}

// write keeps an event in the transcript under its turn; a failed write
// loses that line of history, not the turn.
func (t *turnState) write(e agent.Event) {
	e.TurnID = t.io.TurnID
	_ = t.file.event(e)
}

func (t *turnState) flushText() {
	if t.text.Len() > 0 {
		t.write(agent.Event{Kind: agent.EventText, Text: t.text.String()})
		t.text.Reset()
	}
}

func (t *turnState) flushReasoning() {
	if t.reasoning.Len() > 0 {
		t.write(agent.Event{Kind: agent.EventReasoning, Text: t.reasoning.String()})
		t.reasoning.Reset()
	}
}

// finish writes what is still buffered once the prompt has its answer.
func (t *turnState) finish() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.flushReasoning()
	t.flushText()
}

func (t *turnState) result(stopReason string) agent.Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	usage := t.usage
	return agent.Result{StopReason: stopReason, Usage: &usage}
}

// blockText is a content block's text; other kinds have none to show.
func blockText(raw json.RawMessage) string {
	var b contentBlock
	if json.Unmarshal(raw, &b) != nil || b.Type != "text" {
		return ""
	}
	return b.Text
}

// toolOutput is a finished call's text, its diffs, or else its raw output.
func toolOutput(u update) string {
	var items []toolContent
	_ = json.Unmarshal(u.Content, &items)
	var parts []string
	for _, item := range items {
		switch item.Type {
		case "content":
			if item.Content.Type == "text" && item.Content.Text != "" {
				parts = append(parts, item.Content.Text)
			}
		case "diff":
			parts = append(parts, item.Path+"\n"+item.NewText)
		}
	}
	out := strings.Join(parts, "\n")
	if out == "" && present(u.RawOutput) {
		var s string
		if json.Unmarshal(u.RawOutput, &s) == nil {
			out = s
		} else {
			out = string(u.RawOutput)
		}
	}
	if len(out) > toolOutputLimit {
		out = out[:toolOutputLimit]
	}
	return out
}

func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}
