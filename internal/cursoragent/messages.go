package cursoragent

import "encoding/json"

type Initialization struct {
	ProtocolVersion   int `json:"protocolVersion"`
	AgentCapabilities struct {
		LoadSession        bool `json:"loadSession"`
		PromptCapabilities struct {
			Image bool `json:"image"`
		} `json:"promptCapabilities"`
	} `json:"agentCapabilities"`
}
type SessionParams struct {
	SessionID  string      `json:"sessionId,omitempty"`
	Cwd        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}
type MCPServer struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Headers []Header `json:"headers"`
}
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type Session struct {
	SessionID string `json:"sessionId"`
	Modes     struct {
		CurrentModeID  string `json:"currentModeId"`
		AvailableModes []Mode `json:"availableModes"`
	} `json:"modes"`
	Models struct {
		CurrentModelID  string  `json:"currentModelId"`
		AvailableModels []Model `json:"availableModels"`
	} `json:"models"`
}
type Mode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}
type Model struct {
	ModelID     string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description"`
}
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
}
type ToolContent struct {
	Type    string  `json:"type"`
	Content Content `json:"content"`
	Path    string  `json:"path,omitempty"`
	OldText string  `json:"oldText,omitempty"`
	NewText string  `json:"newText,omitempty"`
}
type ToolCall struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage `json:"rawOutput,omitempty"`
	Content    []ToolContent   `json:"content,omitempty"`
}
type Update struct {
	SessionUpdate string  `json:"sessionUpdate"`
	Content       Content `json:"content"`
	ToolCallID    string  `json:"toolCallId"`
	Title         string  `json:"title"`
}

// Tool updates have array content; message updates have a single content block.
type SessionUpdate struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}
type PromptResult struct {
	StopReason string `json:"stopReason"`
}
type Permission struct {
	SessionID string             `json:"sessionId"`
	ToolCall  ToolCall           `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}
type PermissionResponse struct {
	Outcome PermissionOutcome `json:"outcome"`
}
type Question struct {
	ID            string           `json:"id"`
	Prompt        string           `json:"prompt"`
	Options       []QuestionOption `json:"options"`
	AllowMultiple bool             `json:"allowMultiple"`
}
type QuestionOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}
type Questions struct {
	ToolCallID string     `json:"toolCallId"`
	Title      string     `json:"title"`
	Questions  []Question `json:"questions"`
}
type QuestionAnswer struct {
	QuestionID        string   `json:"questionId"`
	SelectedOptionIDs []string `json:"selectedOptionIds"`
}
type QuestionOutcome struct {
	Outcome string           `json:"outcome"`
	Answers []QuestionAnswer `json:"answers,omitempty"`
}
type QuestionResponse struct {
	Outcome QuestionOutcome `json:"outcome"`
}
type Plan struct {
	ToolCallID string `json:"toolCallId"`
	Name       string `json:"name"`
	Overview   string `json:"overview"`
	Plan       string `json:"plan"`
}
type PlanOutcome struct {
	Outcome string `json:"outcome"`
}
type PlanResponse struct {
	Outcome PlanOutcome `json:"outcome"`
}
