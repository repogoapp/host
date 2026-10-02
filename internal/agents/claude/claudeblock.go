package claude

import (
	"encoding/json"
	"strings"

	"github.com/bytedance/sonic"
)

// claudeBlock is one content block of a message in Claude's transcript.
type claudeBlock struct {
	Type string `json:"type"`

	Text     string `json:"text"`
	Thinking string `json:"thinking"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`

	// image: a URL, or inline base64
	Source struct {
		Type      string `json:"type"`
		URL       string `json:"url"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

// contentText flattens a tool_result's content, a bare string on some results
// and an array of blocks on others.
func (b claudeBlock) contentText() string { return flattenBlocks(b.Content) }

// claudeContent is a message's content: a bare string or an array of blocks.
type claudeContent struct {
	Blocks []claudeBlock
	Text   string
}

func (c *claudeContent) UnmarshalJSON(data []byte) error {
	var s string
	if err := sonic.Unmarshal(data, &s); err == nil {
		c.Text = s
		return nil
	}
	// A shape we do not recognize yields no blocks rather than an error: a
	// single odd line must never abort reading a whole transcript.
	_ = sonic.Unmarshal(data, &c.Blocks)
	return nil
}

// flattenBlocks is a tool result's text. An image block reads "[image]", as in
// codex's flattenParts, so a result that was only a picture is not blank.
func flattenBlocks(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := sonic.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []claudeBlock
	if err := sonic.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			switch b.Type {
			case "text":
				sb.WriteString(b.Text)
			case "image":
				sb.WriteString("[image]")
			}
		}
		return sb.String()
	}
	return string(raw)
}
