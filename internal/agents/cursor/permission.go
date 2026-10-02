package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/cursoragent"
)

func (s *liveSession) respond(ctx context.Context, r cursoragent.Request) (any, error) {
	switch r.Method {
	case "session/request_permission":
		var p cursoragent.Permission
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		return s.permissionRequest(ctx, p)
	case "cursor/ask_question", "_cursor/ask_question":
		var p cursoragent.Questions
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		return s.questionRequest(ctx, p)
	case "cursor/create_plan", "_cursor/create_plan":
		var p cursoragent.Plan
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		a := agent.Approval{CallID: p.ToolCallID, Title: p.Name, Kind: "plan", Input: r.Params, Options: []agent.ApprovalOption{
			{OptionID: "accepted", Name: "Approve plan", Kind: "allow_once"}, {OptionID: "rejected", Name: "Reject plan", Kind: "reject_once"},
		}}
		response := cursoragent.PlanResponse{Outcome: cursoragent.PlanOutcome{Outcome: "cancelled"}}
		raw, err := s.ask(ctx, a)
		if err != nil {
			return response, nil
		}
		var pick string
		if json.Unmarshal(raw, &pick) != nil || (pick != "accepted" && pick != "rejected") {
			return response, errors.New("plan option was not offered")
		}
		response.Outcome.Outcome = pick
		return response, nil
	default:
		return nil, &cursoragent.Error{Code: -32601, Message: "unsupported Cursor request: " + r.Method}
	}
}
func (s *liveSession) permissionRequest(ctx context.Context, p cursoragent.Permission) (cursoragent.PermissionResponse, error) {
	response := cursoragent.PermissionResponse{Outcome: cursoragent.PermissionOutcome{Outcome: "cancelled"}}
	s.mu.Lock()
	if s.io == nil || s.closed || p.SessionID != s.id {
		s.mu.Unlock()
		return response, nil
	}
	mode := s.permission
	input := p.ToolCall.RawInput
	if len(input) == 0 {
		input = s.tools[p.ToolCall.ToolCallID].RawInput
	}
	s.mu.Unlock()
	a := agent.Approval{CallID: p.ToolCall.ToolCallID, Title: p.ToolCall.Title, Kind: p.ToolCall.Kind, Input: input}
	for _, o := range p.Options {
		a.Options = append(a.Options, agent.ApprovalOption{OptionID: o.OptionID, Name: o.Name, Kind: o.Kind})
	}
	var pick string
	automatic := mode == agent.PermissionFullAccess || (mode == agent.PermissionAutoAcceptEdits && p.ToolCall.Kind == "edit")
	if automatic {
		for _, o := range p.Options {
			if o.Kind == "allow_once" {
				pick = o.OptionID
				break
			}
		}
	}
	if pick == "" {
		answer, err := s.ask(ctx, a)
		if err != nil {
			s.markDenied(p.ToolCall.ToolCallID)
			return response, nil
		}
		if err = json.Unmarshal(answer, &pick); err != nil {
			s.markDenied(p.ToolCall.ToolCallID)
			return response, err
		}
	}
	for _, o := range p.Options {
		if o.OptionID == pick {
			if o.Kind != "allow_once" && o.Kind != "allow_always" {
				s.markDenied(p.ToolCall.ToolCallID)
			}
			response.Outcome = cursoragent.PermissionOutcome{Outcome: "selected", OptionID: pick}
			return response, nil
		}
	}
	s.markDenied(p.ToolCall.ToolCallID)
	return response, errors.New("approval option was not offered")
}
func (s *liveSession) markDenied(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.denied != nil {
		s.denied[id] = true
	}
}
func (s *liveSession) questionRequest(ctx context.Context, p cursoragent.Questions) (cursoragent.QuestionResponse, error) {
	response := cursoragent.QuestionResponse{Outcome: cursoragent.QuestionOutcome{Outcome: "cancelled"}}
	a := agent.Approval{CallID: p.ToolCallID, Title: p.Title, Kind: agent.ApprovalQuestion}
	for _, q := range p.Questions {
		question := agent.Question{ID: q.ID, Text: q.Prompt, MultiSelect: q.AllowMultiple, Options: []agent.QuestionOption{}}
		seen := map[string]bool{}
		for _, o := range q.Options {
			if seen[o.Label] {
				return response, errors.New("Cursor question has ambiguous option labels")
			}
			seen[o.Label] = true
			question.Options = append(question.Options, agent.QuestionOption{Label: o.Label})
		}
		a.Questions = append(a.Questions, question)
	}
	raw, err := s.ask(ctx, a)
	if err != nil {
		return response, nil
	}
	var answers map[string]json.RawMessage
	if err = json.Unmarshal(raw, &answers); err != nil {
		return response, err
	}
	out := []cursoragent.QuestionAnswer{}
	for _, q := range p.Questions {
		var labels []string
		if json.Unmarshal(answers[q.ID], &labels) != nil {
			var label string
			if json.Unmarshal(answers[q.ID], &label) != nil {
				return response, fmt.Errorf("missing answer to %s", q.ID)
			}
			labels = []string{label}
		}
		if len(labels) == 0 || (!q.AllowMultiple && len(labels) != 1) {
			return response, errors.New("invalid number of question selections")
		}
		answer := cursoragent.QuestionAnswer{QuestionID: q.ID, SelectedOptionIDs: []string{}}
		selected := map[string]bool{}
		for _, label := range labels {
			found := false
			for _, o := range q.Options {
				if o.Label == label {
					if selected[o.ID] {
						return response, errors.New("duplicate question selection")
					}
					selected[o.ID] = true
					answer.SelectedOptionIDs = append(answer.SelectedOptionIDs, o.ID)
					found = true
					break
				}
			}
			if !found {
				return response, fmt.Errorf("question option %q was not offered", label)
			}
		}
		out = append(out, answer)
	}
	response.Outcome = cursoragent.QuestionOutcome{Outcome: "answered", Answers: out}
	return response, nil
}
