package cursor

import (
	"encoding/json"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/cursoragent"
)

func (s *liveSession) handle(n cursoragent.Notification) error {
	if n.Method != "session/update" {
		return nil
	}
	var frame cursoragent.SessionUpdate
	if err := json.Unmarshal(n.Params, &frame); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Replay populates the snapshot without emitting historical events into a live turn.
	if (s.io == nil && !s.replaying) || frame.SessionID != s.id {
		return nil
	}
	var tag struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(frame.Update, &tag); err != nil {
		return err
	}
	switch tag.SessionUpdate {
	case "user_message_chunk":
		if !s.replaying {
			return nil
		}
		var u cursoragent.Update
		if err := json.Unmarshal(frame.Update, &u); err != nil {
			return err
		}
		if u.Content.Type == "text" {
			s.replayUserLocked(u.Content.Text)
		}
		if u.Content.Type == "image" {
			s.replayUserLocked("\n[Image attachment]")
		}
		return nil
	case "agent_message_chunk", "agent_thought_chunk":
		var u cursoragent.Update
		if err := json.Unmarshal(frame.Update, &u); err != nil {
			return err
		}
		if u.Content.Type != "text" {
			return nil
		}
		kind := agent.EventText
		if tag.SessionUpdate == "agent_thought_chunk" {
			kind = agent.EventReasoning
		}
		return s.emitLocked(agent.Event{Kind: kind, Text: u.Content.Text})
	case "tool_call", "tool_call_update":
		var u cursoragent.ToolCall
		if err := json.Unmarshal(frame.Update, &u); err != nil {
			return err
		}
		if u.ToolCallID == "" {
			return nil
		}
		previous, known := s.tools[u.ToolCallID]
		if u.Title != "" {
			previous.Title = u.Title
		}
		if u.Kind != "" {
			previous.Kind = u.Kind
		}
		if u.Status != "" {
			previous.Status = u.Status
		}
		if u.RawInput != nil {
			previous.RawInput = u.RawInput
		}
		if u.RawOutput != nil {
			previous.RawOutput = u.RawOutput
		}
		if u.Content != nil {
			previous.Content = u.Content
		}
		previous.ToolCallID = u.ToolCallID
		s.tools[u.ToolCallID] = previous
		tool := agent.ToolCall{CallID: u.ToolCallID, Name: previous.Title, Input: previous.RawInput}
		if tool.Name == "" {
			tool.Name = previous.Kind
		}
		if !known {
			if err := s.emitLocked(agent.Event{Kind: agent.EventToolCall, Tool: &tool}); err != nil {
				return err
			}
		}
		if (previous.Status == "completed" || previous.Status == "failed") && !s.finished[u.ToolCallID] {
			s.finished[u.ToolCallID] = true
			tool.IsError = previous.Status == "failed" || s.denied[u.ToolCallID]
			tool.Output = toolOutput(previous)
			if s.denied[u.ToolCallID] {
				tool.Output = "Permission denied"
			}
			return s.emitLocked(agent.Event{Kind: agent.EventToolResult, Tool: &tool})
		}
	}
	return nil
}
func toolOutput(t cursoragent.ToolCall) string {
	var text []string
	for _, c := range t.Content {
		if c.Type == "content" && c.Content.Type == "text" {
			text = append(text, c.Content.Text)
		}
	}
	if len(text) > 0 {
		return strings.Join(text, "\n")
	}
	if len(t.RawOutput) > 0 {
		return string(t.RawOutput)
	}
	return ""
}
