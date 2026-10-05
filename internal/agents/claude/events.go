package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/claudecode"
)

type streamBlock struct{ kind, text string }

func (s *liveSession) handleMessage(m claudecode.Message) {
	s.mu.Lock()
	s.lastActivity = time.Now()
	if m.Type == "system" {
		s.updateTaskLocked(m)
		if m.Subtype == "session_state_changed" {
			s.working = m.State == "running"
		}
		if m.Subtype == "session_state_changed" && m.State == "running" {
			s.owedIdle = 0
		}
	}
	io := s.io
	if io == nil || s.outcome == nil {
		if m.Subtype == "session_state_changed" && m.State == "idle" && s.owedIdle > 0 {
			s.owedIdle--
		}
		s.mu.Unlock()
		return
	}
	if !s.promptSent {
		s.mu.Unlock()
		return
	}
	if m.SessionID != "" && m.SessionID != s.id && m.ParentToolUseID == "" {
		outcome := s.outcome
		s.outcome = nil
		s.mu.Unlock()
		outcome <- turnOutcome{result: agent.Result{SessionID: s.id}, err: fmt.Errorf("Claude changed session identity from %s to %s", s.id, m.SessionID)}
		return
	}
	if m.Type == "assistant" && m.UUID != "" {
		if s.seenMessages == nil {
			s.seenMessages = map[string]bool{}
		}
		if s.seenMessages[m.UUID] {
			s.mu.Unlock()
			return
		}
		s.seenMessages[m.UUID] = true
	}
	if m.Type == "user" && m.UUID == s.promptID {
		s.promptEchoed = true
	}
	var events []agent.Event
	if m.ParentToolUseID == "" {
		switch m.Type {
		case "system":
			if m.Subtype == "local_command_output" {
				events = append(events, agent.Event{Kind: agent.EventText, Text: flattenBlocks(m.Content)})
			}
		case "stream_event":
			events = s.streamLocked(m.Event)
		case "assistant":
			events = s.assistantLocked(m.Message)
		case "user":
			var resumed struct {
				AgentID string `json:"resumedAgentId"`
			}
			if json.Unmarshal(m.ToolUseResult, &resumed) == nil && resumed.AgentID != "" {
				task := s.tasks[resumed.AgentID]
				if task == nil {
					task = &liveTask{}
					s.tasks[resumed.AgentID] = task
				}
				task.terminal = false
				task.subagent = true
				task.owner = io.TurnID
			}
			for _, block := range m.Message.Content {
				if block.Type != "tool_result" || s.seenResults[block.ToolUseID] {
					continue
				}
				s.seenResults[block.ToolUseID] = true
				if block.ToolUseID == s.planRejected && block.IsError {
					s.planRejectionSeen = true
				}
				name := s.calls[block.ToolUseID]
				if name == "" {
					continue
				}
				events = append(events, agent.Event{Kind: agent.EventToolResult, Tool: &agent.ToolCall{CallID: block.ToolUseID, Name: name, Output: flattenBlocks(block.Content), IsError: block.IsError}})
			}
		}
	}
	var completion *turnOutcome
	autonomous := slices.Contains([]string{"task-notification", "peer", "coordinator", "observer", "observer-activity"}, m.Origin.Kind)
	if m.Type == "result" && autonomous {
		if s.deferred != nil && m.NumTurns > 0 && !s.awaitingSubagentsLocked() {
			completion = s.deferred
			s.owedIdle++
		}
	}
	if m.Type == "result" && !autonomous && m.ParentToolUseID == "" && (m.UserMessageUUID == "" || m.UserMessageUUID == s.promptID) {
		s.usage.InputTokens += m.Usage.InputTokens
		s.usage.OutputTokens += m.Usage.OutputTokens
		s.usage.CacheReadTokens += m.Usage.CacheReadTokens
		s.usage.CacheCreationTokens += m.Usage.CacheCreationTokens
		s.usage.CostUSD = m.TotalCostUSD
		if model, ok := m.ModelUsage[s.lastModel]; ok {
			s.usage.ContextSize = model.ContextWindow
		}
		usage := s.usage
		stop := "end_turn"
		if m.StopReason == "max_tokens" || m.StopReason == "refusal" {
			stop = m.StopReason
		}
		if s.planRejected != "" {
			stop = "cancelled"
		}
		err := resultError(m)
		if m.StopReason == "max_tokens" {
			err = nil
		}
		if s.planRejectionSeen && m.Subtype == "error_during_execution" && strings.Contains(strings.Join(m.Errors, " "), "ede_diagnostic") {
			err = nil
		}
		result := turnOutcome{result: agent.Result{SessionID: s.id, StopReason: stop, Usage: &usage}, err: err}
		if !s.sawText && m.Usage.OutputTokens == 0 && m.Result != "" && !m.IsError {
			events = append(events, agent.Event{Kind: agent.EventText, Text: m.Result})
		}
		if result.err == nil && s.awaitingSubagentsLocked() {
			s.deferred = &result
		} else {
			completion = &result
			s.owedIdle++
		}
	}
	if m.Type == "system" && m.Subtype == "session_state_changed" && m.State == "idle" {
		switch {
		case s.deferred != nil:
			if !s.awaitingSubagentsLocked() {
				completion = s.deferred
			}
		case s.owedIdle > 0:
			s.owedIdle--
		case s.promptEchoed:
			completion = &turnOutcome{result: agent.Result{SessionID: s.id}, err: errors.New("Claude went idle without a result for the accepted prompt")}
		}
	}
	outcome := s.outcome
	if completion != nil {
		s.outcome = nil
		s.deferred = nil
	}
	busy := s.pending > 0 || s.awaitingSubagentsLocked()
	for _, event := range events {
		if event.Kind == agent.EventText && event.Text != "" {
			s.sawText = true
		}
	}
	s.mu.Unlock()
	if io.Activity != nil {
		io.Activity(busy)
	}
	for _, event := range events {
		io.Emit(event)
	}
	if completion != nil {
		outcome <- *completion
	}
}

func (s *liveSession) streamLocked(e claudecode.StreamEvent) []agent.Event {
	if e.Type == "message_start" {
		s.messageID = e.Message.ID
		s.blocks[e.Message.ID] = map[int]*streamBlock{}
		s.lastModel = e.Message.Model
		s.contextLocked(e.Message.Usage)
	}
	id := s.messageID
	if s.blocks[id] == nil {
		s.blocks[id] = map[int]*streamBlock{}
	}
	switch e.Type {
	case "content_block_start":
		s.blocks[id][e.Index] = &streamBlock{kind: e.ContentBlock.Type}
		// A tool_use starts with an empty input that deltas fill in; the call
		// is recorded from the assistant message, which carries it whole.
		if e.ContentBlock.Type == "tool_use" {
			return nil
		}
		return s.blockLocked(id, e.Index, e.ContentBlock)
	case "content_block_delta":
		block := s.blocks[id][e.Index]
		if block == nil {
			return nil
		}
		text, kind := e.Delta.Text, agent.EventText
		if e.Delta.Type == "thinking_delta" {
			text = e.Delta.Thinking
			kind = agent.EventReasoning
		}
		if text != "" {
			block.text += text
			return []agent.Event{{Kind: kind, Text: text}}
		}
	}
	return nil
}

func (s *liveSession) assistantLocked(m claudecode.Assistant) []agent.Event {
	s.lastModel = m.Model
	s.contextLocked(m.Usage)
	id := s.messageID
	streamed := s.blocks[id]
	indices := make([]int, 0, len(streamed))
	for index, block := range streamed {
		if block.kind == "text" || block.kind == "thinking" {
			indices = append(indices, index)
		}
	}
	slices.Sort(indices)
	position := 0
	var events []agent.Event
	for i, block := range m.Content {
		if block.Type == "text" || block.Type == "thinking" {
			text, kind := block.Text, agent.EventText
			if block.Type == "thinking" {
				text = block.Thinking
				kind = agent.EventReasoning
			}
			if position < len(indices) {
				previous := streamed[indices[position]]
				if previous.kind == block.Type && previous.text != "" && strings.HasPrefix(text, previous.text) {
					text = strings.TrimPrefix(text, previous.text)
					position++
				}
			}
			if text != "" {
				events = append(events, agent.Event{Kind: kind, Text: text})
			}
		} else {
			events = append(events, s.blockLocked(m.ID, i, block)...)
		}
	}
	s.blocks[id] = map[int]*streamBlock{}
	return events
}

func (s *liveSession) contextLocked(u claudecode.Usage) {
	used := u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
	if used > 0 {
		s.usage.ContextUsed = used
	}
}

func (s *liveSession) blockLocked(id string, index int, b claudecode.ContentBlock) []agent.Event {
	switch b.Type {
	case "text", "thinking":
		text, kind := b.Text, agent.EventText
		if b.Type == "thinking" {
			text = b.Thinking
			kind = agent.EventReasoning
		}
		previous := s.blocks[id][index]
		if previous == nil {
			previous = &streamBlock{kind: b.Type}
			s.blocks[id][index] = previous
		}
		previous.text += text
		if text != "" {
			return []agent.Event{{Kind: kind, Text: text}}
		}
	case "tool_use":
		if b.ID == "" || s.calls[b.ID] != "" {
			return nil
		}
		name := liveToolName(b.Name)
		if name == "" {
			return nil
		}
		s.calls[b.ID] = name
		return []agent.Event{{Kind: agent.EventToolCall, Tool: &agent.ToolCall{CallID: b.ID, Name: name, Input: b.Input}}}
	}
	return nil
}

func liveToolName(name string) string {
	switch name {
	case "Task", "Agent":
		return name
	case "Bash", "PowerShell", "BashOutput", "KillShell":
		return "execute"
	case "Read":
		return "read"
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return "edit"
	case "Glob", "Grep":
		return "search"
	case "WebFetch", "WebSearch":
		return "fetch"
	case "TodoWrite", "TaskCreate", "TaskUpdate", "TaskList", "TaskGet":
		return ""
	case "EnterPlanMode", "ExitPlanMode":
		return "switch_mode"
	default:
		return "other"
	}
}
