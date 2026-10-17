package acp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/repogoapp/host/internal/agent"
)

// question is one form field as the phone asks it, with each choice's title
// mapped back to the value the agent reads.
type question struct {
	agent.Question
	values map[string]string
}

// readForm turns a form's fields into questions, in the order the agent
// wrote them; a field without a title asks the form's message.
func readForm(schema json.RawMessage, message string) ([]question, error) {
	var top struct {
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &top); err != nil || len(top.Properties) == 0 {
		return nil, errors.New("the form has no fields")
	}
	dec := json.NewDecoder(bytes.NewReader(top.Properties))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out []question
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var f formField
		if err := dec.Decode(&f); err != nil {
			return nil, err
		}
		choices := f.OneOf
		if f.Type == "array" {
			choices = f.Items.AnyOf
		}
		q := question{
			Question: agent.Question{
				ID: key.(string), Text: cmp.Or(f.Title, f.Description, message),
				MultiSelect: f.Type == "array", Options: []agent.QuestionOption{},
			},
			values: map[string]string{},
		}
		for _, c := range choices {
			label := cmp.Or(c.Title, c.Const)
			q.Options = append(q.Options, agent.QuestionOption{Label: label, Description: c.Description})
			q.values[label] = c.Const
		}
		out = append(out, q)
	}
	return out, nil
}

// content is the phone's answers as the form's values: a choice's value, a
// list of them for several, or the typed text.
func content(questions []question, answer json.RawMessage) (map[string]any, error) {
	var answers map[string]json.RawMessage
	if err := json.Unmarshal(answer, &answers); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, q := range questions {
		picks := answerValues(answers[q.ID])
		if len(q.Options) == 0 {
			out[q.ID] = strings.Join(picks, "\n")
			continue
		}
		values := []string{}
		for _, pick := range picks {
			if v, ok := q.values[pick]; ok {
				values = append(values, v)
			}
		}
		switch {
		case q.MultiSelect:
			out[q.ID] = values
		case len(values) > 0:
			out[q.ID] = values[0]
		}
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

// ask puts an agent's form to the phone as a question card and reads the
// answers back; a turn that ended first, or a form it can't show, cancels.
func (t *turnState) ask(ctx context.Context, requestID string, p elicitParams) elicitResult {
	cancelled := elicitResult{Action: "cancel"}
	if p.Mode != "form" || t.io.Ask == nil {
		return elicitResult{Action: "decline"}
	}
	questions, err := readForm(p.RequestedSchema, p.Message)
	if err != nil {
		return elicitResult{Action: "decline"}
	}
	asked := make([]agent.Question, len(questions))
	for i, q := range questions {
		asked[i] = q.Question
	}
	// A question sits on the row of the tool call asking it: the one the form
	// names, else the turn's call still waiting (eve names none).
	callID := cmp.Or(p.ToolCallID, t.waitingTool(), "question-"+t.io.TurnID+"-"+requestID)
	answer, err := t.io.Ask(ctx, agent.Approval{CallID: callID, Title: p.Message, Kind: agent.ApprovalQuestion, Questions: asked})
	if err != nil {
		return cancelled
	}
	values, err := content(questions, answer)
	if err != nil {
		return cancelled
	}
	return elicitResult{Action: "accept", Content: values}
}
