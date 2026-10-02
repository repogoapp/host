package codex

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/codexappserver"
)

// userInput is the model's request_user_input as a question card. Typed text
// rides under the question's CustomID when it also has options.
func (s *liveSession) userInput(ctx context.Context, p codexappserver.UserInput) (codexappserver.UserInputResponse, error) {
	questions := make([]agent.Question, 0, len(p.Questions))
	for _, q := range p.Questions {
		question := agent.Question{ID: q.ID, Header: q.Header, Text: q.Question, Options: []agent.QuestionOption{}}
		for _, o := range q.Options {
			question.Options = append(question.Options, agent.QuestionOption{Label: o.Label, Description: o.Description})
		}
		if q.IsOther && len(q.Options) > 0 {
			question.CustomID = q.ID + "_other"
		}
		questions = append(questions, question)
	}
	title := "Please answer the following questions."
	if len(questions) == 1 {
		title = questions[0].Text
	}
	out := codexappserver.UserInputResponse{Answers: map[string]codexappserver.UserInputAnswer{}}
	answer, err := s.ask(ctx, agent.Approval{CallID: p.ItemID, Title: title, Kind: agent.ApprovalQuestion, Questions: questions})
	if err != nil {
		return out, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(answer, &values); err != nil {
		return out, err
	}
	for _, q := range questions {
		picks := answerValues(values[q.ID])
		if q.CustomID != "" {
			picks = append(picks, answerValues(values[q.CustomID])...)
		}
		out.Answers[q.ID] = codexappserver.UserInputAnswer{Answers: picks}
	}
	return out, nil
}

// answerValues reads one answer, a label or a list of them, without blanks.
func answerValues(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		var one string
		if json.Unmarshal(raw, &one) != nil {
			return nil
		}
		list = []string{one}
	}
	out := list[:0]
	for _, v := range list {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// elicitation is an MCP server asking through Codex. A form with fields is a
// question card; one with none is Codex asking to run the server's tool.
// RepoGo's own server is the user's standing yes.
func (s *liveSession) elicitation(ctx context.Context, requestID string, p codexappserver.Elicitation) (codexappserver.ElicitationResponse, error) {
	accept := codexappserver.ElicitationResponse{Action: "accept", Content: json.RawMessage(`{}`)}
	if p.ServerName == agent.ToolsServer {
		return accept, nil
	}
	if p.Mode != "form" {
		return codexappserver.ElicitationResponse{Action: "decline"}, nil
	}
	callID := "elicitation:" + p.ServerName + ":" + requestID
	if questions, err := agent.ParseForm(p.RequestedSchema); err == nil {
		for i := range questions {
			if questions[i].Text == "" {
				questions[i].Text = p.Message
			}
		}
		answer, err := s.ask(ctx, agent.Approval{CallID: callID, Title: p.Message, Kind: agent.ApprovalQuestion, Questions: questions})
		if err != nil {
			return codexappserver.ElicitationResponse{Action: "cancel"}, nil
		}
		return codexappserver.ElicitationResponse{Action: "accept", Content: answer}, nil
	}
	var meta struct {
		ToolParams json.RawMessage `json:"tool_params"`
	}
	_ = json.Unmarshal(p.Meta, &meta)
	offered := []decision{
		{option: agent.ApprovalOption{OptionID: "accept", Name: "Yes", Kind: "allow_once"}, reply: "accept"},
		{option: agent.ApprovalOption{OptionID: "decline", Name: "No", Kind: "reject_once"}, reply: "decline"},
	}
	picked, err := s.decide(ctx, agent.Approval{CallID: callID, Title: p.Message, Kind: "mcp", Input: meta.ToolParams}, offered)
	if err != nil {
		return codexappserver.ElicitationResponse{}, err
	}
	switch picked {
	case "accept":
		return accept, nil
	case "decline":
		return codexappserver.ElicitationResponse{Action: "decline"}, nil
	default:
		return codexappserver.ElicitationResponse{Action: "cancel"}, nil
	}
}
