package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/claudecode"
)

func (s *liveSession) askUserQuestion(ctx context.Context, p claudecode.PermissionRequest) (claudecode.PermissionResult, error) {
	var input struct {
		Questions []struct {
			Question    string                 `json:"question"`
			Header      string                 `json:"header"`
			MultiSelect bool                   `json:"multiSelect"`
			Options     []agent.QuestionOption `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(p.Input, &input); err != nil {
		return claudecode.PermissionResult{}, err
	}
	questions := make([]agent.Question, 0, len(input.Questions))
	for i, q := range input.Questions {
		questions = append(questions, agent.Question{ID: fmt.Sprintf("question_%d", i), CustomID: fmt.Sprintf("question_%d_custom", i), Text: q.Question, Header: q.Header, MultiSelect: q.MultiSelect, Options: q.Options})
	}
	title := "Please answer the following questions."
	if len(questions) == 1 {
		title = questions[0].Text
	}
	answer, err := s.ask(ctx, agent.Approval{CallID: p.ToolUseID, Title: title, Kind: agent.ApprovalQuestion, Questions: questions})
	if err != nil {
		return denied(p, "Question cancelled", true), nil
	}
	var values map[string]json.RawMessage
	if err = json.Unmarshal(answer, &values); err != nil {
		return claudecode.PermissionResult{}, err
	}
	answers := map[string]string{}
	annotations := map[string]map[string]string{}
	for i, q := range questions {
		var custom string
		_ = json.Unmarshal(values[q.CustomID], &custom)
		custom = strings.TrimSpace(custom)
		if q.MultiSelect {
			var picks []string
			_ = json.Unmarshal(values[q.ID], &picks)
			if custom != "" {
				picks = append(picks, custom)
			}
			for j, pick := range picks {
				if strings.ContainsAny(pick, ",\"") {
					quoted, _ := json.Marshal(pick)
					picks[j] = string(quoted)
				}
			}
			if len(picks) > 0 {
				answers[input.Questions[i].Question] = strings.Join(picks, ", ")
			}
		} else {
			var pick string
			_ = json.Unmarshal(values[q.ID], &pick)
			if pick == "" {
				pick = custom
			} else if custom != "" {
				annotations[q.Text] = map[string]string{"notes": custom}
			}
			if pick != "" {
				answers[q.Text] = pick
			}
		}
	}
	var updated map[string]json.RawMessage
	if err = json.Unmarshal(p.Input, &updated); err != nil {
		return claudecode.PermissionResult{}, err
	}
	updated["answers"], _ = json.Marshal(answers)
	if len(annotations) > 0 {
		updated["annotations"], _ = json.Marshal(annotations)
	}
	raw, err := json.Marshal(updated)
	return claudecode.PermissionResult{Behavior: "allow", UpdatedInput: raw, ToolUseID: p.ToolUseID}, err
}

func (s *liveSession) elicitation(ctx context.Context, e claudecode.ElicitationRequest) (claudecode.ElicitationResult, error) {
	if e.Mode == "url" {
		return claudecode.ElicitationResult{Action: "decline"}, nil
	}
	questions, err := agent.ParseForm(e.Schema)
	if err != nil {
		return claudecode.ElicitationResult{}, err
	}
	for i := range questions {
		if questions[i].Text == "" {
			questions[i].Text = e.Message
		}
	}
	answer, err := s.ask(ctx, agent.Approval{CallID: uuid.NewString(), Title: e.Message, Kind: agent.ApprovalQuestion, Questions: questions})
	if err != nil {
		return claudecode.ElicitationResult{Action: "cancel"}, nil
	}
	return claudecode.ElicitationResult{Action: "accept", Content: answer}, nil
}
