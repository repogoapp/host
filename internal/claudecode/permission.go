package claudecode

import (
	"context"
	"encoding/json"
)

type PermissionRequest struct {
	ToolName            string            `json:"tool_name"`
	Input               json.RawMessage   `json:"input"`
	ToolUseID           string            `json:"tool_use_id"`
	AgentID             string            `json:"agent_id"`
	Suggestions         []json.RawMessage `json:"permission_suggestions"`
	MatchedAskRule      json.RawMessage   `json:"matched_ask_rule"`
	SuppressAlwaysAllow bool              `json:"suppress_always_allow_rule"`
	DefaultToNo         bool              `json:"default_to_no"`
	DisplayName         string            `json:"display_name"`
}

type PermissionResult struct {
	Behavior               string            `json:"behavior"`
	UpdatedInput           json.RawMessage   `json:"updatedInput,omitempty"`
	UpdatedPermissions     []json.RawMessage `json:"updatedPermissions,omitempty"`
	Message                string            `json:"message,omitempty"`
	Interrupt              bool              `json:"interrupt,omitempty"`
	ToolUseID              string            `json:"toolUseID,omitempty"`
	DecisionClassification string            `json:"decisionClassification,omitempty"`
}

type ElicitationRequest struct {
	Server  string          `json:"mcp_server_name"`
	Message string          `json:"message"`
	Mode    string          `json:"mode"`
	Schema  json.RawMessage `json:"requested_schema"`
}

type ElicitationResult struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content,omitempty"`
}

type Handlers struct {
	CanUseTool  func(context.Context, PermissionRequest) (PermissionResult, error)
	Elicitation func(context.Context, ElicitationRequest) (ElicitationResult, error)
}
