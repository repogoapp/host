package agent

import (
	"bytes"
	"encoding/json"
	"errors"
)

// formField is one property of an MCP elicitation schema: a select as a
// titled enum (`oneOf`, or `items.anyOf` for multi-select), or free text.
type formField struct {
	Type        string       `json:"type"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	OneOf       []formOption `json:"oneOf"`
	Items       struct {
		AnyOf []formOption `json:"anyOf"`
	} `json:"items"`
}

type formOption struct {
	Const       string `json:"const"`
	Description string `json:"description"`
}

// ParseForm turns an elicitation schema into questions, in the order the
// agent wrote them: a map would shuffle "1 of 3" past "2 of 3".
func ParseForm(schema json.RawMessage) ([]Question, error) {
	var top struct {
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &top); err != nil || len(top.Properties) == 0 {
		return nil, errors.New("form has no fields")
	}

	dec := json.NewDecoder(bytes.NewReader(top.Properties))
	if _, err := dec.Token(); err != nil { // opening brace
		return nil, err
	}
	var questions []Question
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key := keyTok.(string)
		var f formField
		if err := dec.Decode(&f); err != nil {
			return nil, err
		}
		raw := f.OneOf
		if f.Type == "array" {
			raw = f.Items.AnyOf
		}
		opts := make([]QuestionOption, 0, len(raw))
		for _, o := range raw {
			// The answer echoes the label back, so it must be the const.
			opts = append(opts, QuestionOption{Label: o.Const, Description: o.Description})
		}
		questions = append(questions, Question{
			ID:          key,
			Header:      f.Title,
			Text:        f.Description,
			MultiSelect: f.Type == "array",
			Options:     opts,
		})
	}
	if len(questions) == 0 {
		return nil, errors.New("form has no questions")
	}
	return questions, nil
}
