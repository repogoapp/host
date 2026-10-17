package acp

import "encoding/json"

// The Agent Client Protocol's methods and the version this client speaks.
const (
	protocolVersion = 1

	methodInitialize = "initialize"
	methodNewSession = "session/new"
	methodLoad       = "session/load"
	methodPrompt     = "session/prompt"
	methodCancel     = "session/cancel"
	methodUpdate     = "session/update"
	methodPermission = "session/request_permission"
	methodSetConfig  = "session/set_config_option"
	methodElicit     = "elicitation/create"
)

type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
	ClientInfo         implementation     `json:"clientInfo"`
}

// clientCapabilities offers no file or terminal access: an agent works on the
// environment's disk itself, not through the phone.
type clientCapabilities struct {
	FS       fsCapabilities `json:"fs"`
	Terminal bool           `json:"terminal"`
	// Elicitation offers forms, which is how an agent asks the user a question.
	Elicitation elicitationCapabilities `json:"elicitation"`
}

type elicitationCapabilities struct {
	Form struct{} `json:"form"`
}

type fsCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AgentInfo         *implementation   `json:"agentInfo"`
}

type agentCapabilities struct {
	LoadSession        bool `json:"loadSession"`
	PromptCapabilities struct {
		Image bool `json:"image"`
	} `json:"promptCapabilities"`
	MCPCapabilities struct {
		HTTP bool `json:"http"`
	} `json:"mcpCapabilities"`
}

// sessionParams opens a session (session/new) or reopens one (session/load,
// with SessionID). MCPServers is required, so it is never nil.
type sessionParams struct {
	SessionID  string      `json:"sessionId,omitempty"`
	Cwd        string      `json:"cwd"`
	MCPServers []mcpServer `json:"mcpServers"`
}

// mcpServer is an HTTP server as ACP names one; agent.MCPServer has the
// same fields.
type mcpServer struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Headers []header `json:"headers"`
}

type header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type newSessionResult struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []configOption `json:"configOptions"`
}

// sessionResult is what session/load answers: the reopened session's options.
type sessionResult struct {
	ConfigOptions []configOption `json:"configOptions"`
}

// configOption is one of a session's settings. A select's Options is a flat
// list of choices or a list of groups of them.
type configOption struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Category     string          `json:"category"`
	Type         string          `json:"type"`
	CurrentValue string          `json:"currentValue"`
	Options      json.RawMessage `json:"options"`
}

type configChoice struct {
	Value   string         `json:"value"`
	Name    string         `json:"name"`
	Options []configChoice `json:"options"`
}

// categoryModel marks the option that picks the model.
const categoryModel = "model"

type setConfigParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

// contentBlock is text, an image, or a link to a file on the environment.
type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
}

// Stop reasons a prompt ends with.
const (
	stopEndTurn   = "end_turn"
	stopCancelled = "cancelled"
	stopRefusal   = "refusal"
)

type promptResult struct {
	StopReason string `json:"stopReason"`
}

type cancelParams struct {
	SessionID string `json:"sessionId"`
}

type updateParams struct {
	SessionID string `json:"sessionId"`
	Update    update `json:"update"`
}

// The session updates the host reads; the rest (plans, commands, modes) have
// nowhere to show yet and are skipped.
const (
	updateMessage  = "agent_message_chunk"
	updateThought  = "agent_thought_chunk"
	updateToolCall = "tool_call"
	updateTool     = "tool_call_update"
	updateUsage    = "usage_update"
	updateConfig   = "config_option_update"
)

// update is one session/update. A message chunk fills Content; a tool call
// or its update fills the tool fields; usage fills Used, Size and Cost.
type update struct {
	SessionUpdate string `json:"sessionUpdate"`

	Content json.RawMessage `json:"content"`

	ToolCallID string          `json:"toolCallId"`
	Title      *string         `json:"title"`
	Name       *string         `json:"name"`
	Kind       *string         `json:"kind"`
	Status     *string         `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`

	Used int64 `json:"used"`
	Size int64 `json:"size"`
	Cost *cost `json:"cost"`

	ConfigOptions []configOption `json:"configOptions"`
}

type cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// Tool call statuses.
const (
	statusPending   = "pending"
	statusCompleted = "completed"
	statusFailed    = "failed"
)

// toolContent is one item of a tool call's content: a content block, or a diff.
type toolContent struct {
	Type    string       `json:"type"`
	Content contentBlock `json:"content"`
	Path    string       `json:"path"`
	NewText string       `json:"newText"`
}

type permissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  update             `json:"toolCall"`
	Options   []permissionOption `json:"options"`
}

// permissionOption's Kind is allow_once, allow_always, reject_once or
// reject_always, the same words the phone's approval sheet uses.
type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type permissionResult struct {
	Outcome permissionOutcome `json:"outcome"`
}

// permissionOutcome is "selected" with the picked OptionID, or "cancelled".
type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// elicitParams is an agent asking the user: a form of fields, in the
// session and, when it came from one, the tool call it belongs to.
type elicitParams struct {
	Mode            string          `json:"mode"`
	SessionID       string          `json:"sessionId"`
	ToolCallID      string          `json:"toolCallId"`
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema"`
}

// formField is one field of a form: a choice (oneOf, or items.anyOf for
// several), or free text.
type formField struct {
	Type        string       `json:"type"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	OneOf       []formOption `json:"oneOf"`
	Items       struct {
		AnyOf []formOption `json:"anyOf"`
	} `json:"items"`
}

// formOption is a choice: Const is what the agent reads back, Title what
// the user sees.
type formOption struct {
	Const       string `json:"const"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// elicitResult is "accept" with the answers by field, or "decline" or "cancel".
type elicitResult struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitempty"`
}
