package codex

import (
	"encoding/json"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/codexappserver"
)

// handle folds one notification into the bound turn. Only the chat's own
// thread speaks here: a subagent's thread streams on the same connection,
// and its prose is its own transcript's, not the chat's.
func (s *liveSession) handle(n codexappserver.Notification) {
	var scope struct {
		ThreadID string `json:"threadId"`
	}
	if json.Unmarshal(n.Params, &scope) != nil || (scope.ThreadID != "" && scope.ThreadID != s.id) {
		return
	}
	switch n.Method {
	case codexappserver.MethodAgentMessageDelta:
		var d codexappserver.DeltaNotification
		if json.Unmarshal(n.Params, &d) == nil && d.Delta != "" {
			s.mu.Lock()
			if s.streamed != nil {
				s.streamed[d.ItemID] = true
			}
			s.mu.Unlock()
			s.emit(agent.Event{Kind: agent.EventText, Text: d.Delta})
		}
	case codexappserver.MethodReasoningSummaryDelta, codexappserver.MethodReasoningTextDelta:
		var d codexappserver.DeltaNotification
		if json.Unmarshal(n.Params, &d) == nil && d.Delta != "" {
			s.emit(agent.Event{Kind: agent.EventReasoning, Text: d.Delta})
		}
	case codexappserver.MethodItemStarted:
		var in codexappserver.ItemNotification
		if json.Unmarshal(n.Params, &in) == nil {
			s.itemStarted(in.Item)
		}
	case codexappserver.MethodItemCompleted:
		var in codexappserver.ItemNotification
		if json.Unmarshal(n.Params, &in) == nil {
			s.itemCompleted(in.Item)
		}
	case codexappserver.MethodTokenUsageUpdated:
		var u codexappserver.TokenUsageNotification
		if json.Unmarshal(n.Params, &u) == nil {
			s.record(u)
		}
	case codexappserver.MethodError:
		var e codexappserver.ErrorNotification
		if json.Unmarshal(n.Params, &e) == nil && !e.WillRetry {
			s.mu.Lock()
			s.failure = e.Error.Message
			s.mu.Unlock()
		}
	case codexappserver.MethodTurnCompleted:
		var t codexappserver.TurnNotification
		if json.Unmarshal(n.Params, &t) == nil {
			s.turnCompleted(t.Turn)
		}
	}
}

func (s *liveSession) turnCompleted(turn codexappserver.Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.io == nil:
	case s.turnID == "":
		s.finished[turn.ID] = turn
	case turn.ID == s.turnID:
		s.completeLocked(turn)
	}
}

// itemStarted announces a tool call, remembering its name for the result and
// the files a file change will ask to write.
func (s *liveSession) itemStarted(item codexappserver.Item) {
	call, ok := toolCall(item)
	if !ok {
		return
	}
	s.mu.Lock()
	if s.names == nil {
		s.mu.Unlock()
		return
	}
	s.names[item.ID] = call.Name
	if item.Type == "mcpToolCall" && item.Server == agent.ToolsServer {
		s.own[item.ID] = true
	}
	if item.Type == "fileChange" {
		s.changes[item.ID] = item.Changes
	}
	s.mu.Unlock()
	s.emit(agent.Event{Kind: agent.EventToolCall, Tool: &call})
}

// itemCompleted ends a tool call, or delivers a message that arrived whole
// rather than as deltas. An async question's message stays out of the prose:
// its request_user_input_async call already carries the question.
func (s *liveSession) itemCompleted(item codexappserver.Item) {
	if item.Type == "agentMessage" {
		if item.Delivery == "async" {
			return
		}
		s.mu.Lock()
		streamed := s.streamed != nil && s.streamed[item.ID]
		s.mu.Unlock()
		if !streamed && item.Text != "" {
			s.emit(agent.Event{Kind: agent.EventText, Text: item.Text})
		}
		return
	}
	call, ok := toolCall(item)
	if !ok {
		return
	}
	s.mu.Lock()
	_, started := s.names[item.ID]
	s.mu.Unlock()
	if !started {
		s.itemStarted(item)
	}
	s.emit(agent.Event{Kind: agent.EventToolResult, Tool: &agent.ToolCall{
		CallID: item.ID, Name: call.Name, Output: toolOutput(item), IsError: toolFailed(item),
	}})
}

// toolCall names an item as Codex's rollout does, so a live call reads the
// same as the transcript row that replaces it.
func toolCall(item codexappserver.Item) (agent.ToolCall, bool) {
	call := agent.ToolCall{CallID: item.ID}
	switch item.Type {
	case "commandExecution":
		call.Name = "exec_command"
		call.Input, _ = json.Marshal(map[string]string{"cmd": item.Command, "workdir": item.Cwd})
	case "fileChange":
		call.Name = "apply_patch"
		call.Input, _ = json.Marshal(map[string]json.RawMessage{"changes": item.Changes})
	case "mcpToolCall":
		call.Name = "mcp__" + item.Server + "__" + item.Tool
		call.Input = item.Arguments
	case "dynamicToolCall":
		call.Name = item.Tool
		call.Input = item.Arguments
	case "collabAgentToolCall":
		call.Name = collabName(item.Tool)
	case "webSearch":
		call.Name = "web_search"
		var action struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(item.Action, &action)
		if action.Type == "openPage" || action.Type == "findInPage" {
			call.Name = "web_fetch"
		}
		// The rollout keeps the action as the call's input; so does the live call.
		call.Input = item.Action
		if len(call.Input) == 0 || string(call.Input) == "null" {
			call.Input, _ = json.Marshal(map[string]string{"type": "search", "query": item.Query})
		}
	case "imageView":
		call.Name = "view_image"
		call.Input, _ = json.Marshal(map[string]string{"path": item.Path})
	default:
		return agent.ToolCall{}, false
	}
	return call, true
}

// collabName is a multi-agent call's rollout name: spawnAgent live is
// spawn_agent there, and a bare wait there is a code cell's.
func collabName(tool string) string {
	if tool == "wait" {
		return "wait_agent"
	}
	return snakeCase(tool)
}

func toolOutput(item codexappserver.Item) string {
	switch {
	case item.AggregatedOutput != nil:
		return *item.AggregatedOutput
	case item.Error != nil:
		return item.Error.Message
	case len(item.Result) > 0:
		var result struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(item.Result, &result)
		return contentText(result.Content)
	default:
		return contentText(item.ContentItems)
	}
}

// contentText joins the text parts of an MCP-style content list.
func contentText(raw json.RawMessage) string {
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func toolFailed(item codexappserver.Item) bool {
	return item.Status == "failed" || item.Status == "declined" || item.Error != nil
}

// record adds one model request's tokens to the turn. Codex counts cached
// input inside its input, which the host reports apart.
func (s *liveSession) record(u codexappserver.TokenUsageNotification) {
	last := u.TokenUsage.Last
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.io == nil {
		return
	}
	s.usage.InputTokens += last.InputTokens - last.CachedInputTokens
	s.usage.CacheReadTokens += last.CachedInputTokens
	s.usage.CacheCreationTokens += last.CacheWriteInputTokens
	s.usage.OutputTokens += last.OutputTokens
	s.usage.ContextUsed = last.TotalTokens
	if u.TokenUsage.ModelContextWindow != nil {
		s.usage.ContextSize = *u.TokenUsage.ModelContextWindow
	}
}
