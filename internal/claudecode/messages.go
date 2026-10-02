package claudecode

import "encoding/json"

type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *ImageSource    `json:"source,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type UserMessage struct {
	Type            string  `json:"type"`
	UUID            string  `json:"uuid"`
	SessionID       string  `json:"session_id"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Origin          struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message struct {
		Role    string         `json:"role"`
		Content []ContentBlock `json:"content"`
	} `json:"message"`
}

type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

type Assistant struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	Content    []ContentBlock `json:"content"`
	Usage      Usage          `json:"usage"`
	StopReason string         `json:"stop_reason"`
}

func (m *Assistant) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return nil
	}
	type plain Assistant
	var wire struct {
		*plain
		Content json.RawMessage `json:"content"`
	}
	wire.plain = (*plain)(m)
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if len(wire.Content) == 0 || string(wire.Content) == "null" {
		return nil
	}
	if wire.Content[0] == '"' {
		var text string
		if err := json.Unmarshal(wire.Content, &text); err != nil {
			return err
		}
		m.Content = []ContentBlock{{Type: "text", Text: text}}
		return nil
	}
	return json.Unmarshal(wire.Content, &m.Content)
}

type StreamEvent struct {
	Type         string       `json:"type"`
	Index        int          `json:"index"`
	Message      Assistant    `json:"message"`
	ContentBlock ContentBlock `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
}

// Message retains the envelope so an unknown subtype can be diagnosed without losing the stream.
type Message struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	SessionID       string          `json:"session_id"`
	UUID            string          `json:"uuid"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
	UserMessageUUID string          `json:"user_message_uuid"`
	Message         Assistant       `json:"message"`
	Event           StreamEvent     `json:"event"`
	Result          string          `json:"result"`
	Content         json.RawMessage `json:"content"`
	StopReason      string          `json:"stop_reason"`
	NumTurns        int             `json:"num_turns"`
	Origin          struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Errors       []string `json:"errors"`
	IsError      bool     `json:"is_error"`
	Usage        Usage    `json:"usage"`
	TotalCostUSD float64  `json:"total_cost_usd"`
	ModelUsage   map[string]struct {
		ContextWindow int64 `json:"contextWindow"`
	} `json:"modelUsage"`
	State          string `json:"state"`
	TaskID         string `json:"task_id"`
	ToolUseID      string `json:"tool_use_id"`
	TaskType       string `json:"task_type"`
	SubagentType   string `json:"subagent_type"`
	Status         string `json:"status"`
	Summary        string `json:"summary"`
	IsBackgrounded bool   `json:"is_backgrounded"`
	Ambient        bool   `json:"ambient"`
	SkipTranscript bool   `json:"skip_transcript"`
	Patch          struct {
		Status         string `json:"status"`
		IsBackgrounded *bool  `json:"is_backgrounded"`
	} `json:"patch"`
	Tasks []struct {
		TaskID  string `json:"task_id"`
		Ambient bool   `json:"ambient"`
	} `json:"tasks"`
	ToolUseResult json.RawMessage `json:"tool_use_result"`
	Raw           json.RawMessage `json:"-"`
}
