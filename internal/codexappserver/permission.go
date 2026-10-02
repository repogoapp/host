package codexappserver

import (
	"encoding/json"

	"github.com/repogo/host/internal/stdiorpc"
)

// Request is one request Codex sends, answered by Handlers.Request.
type Request = stdiorpc.Request

// The requests Codex sends when it needs a person.
const (
	MethodCommandApproval     = "item/commandExecution/requestApproval"
	MethodFileChangeApproval  = "item/fileChange/requestApproval"
	MethodPermissionsApproval = "item/permissions/requestApproval"
	MethodUserInput           = "item/tool/requestUserInput"
	MethodElicitation         = "mcpServer/elicitation/request"
)

// CommandApproval asks to run a command. AvailableDecisions lists what may be
// answered, in order: strings, or objects such as an execpolicy amendment.
type CommandApproval struct {
	ThreadID                    string            `json:"threadId"`
	TurnID                      string            `json:"turnId"`
	ItemID                      string            `json:"itemId"`
	ApprovalID                  *string           `json:"approvalId"`
	Command                     *string           `json:"command"`
	Cwd                         *string           `json:"cwd"`
	Reason                      *string           `json:"reason"`
	AvailableDecisions          []json.RawMessage `json:"availableDecisions"`
	ProposedExecpolicyAmendment []string          `json:"proposedExecpolicyAmendment"`
}

// FileChangeApproval asks to apply the fileChange item ItemID.
type FileChangeApproval struct {
	ThreadID  string  `json:"threadId"`
	TurnID    string  `json:"turnId"`
	ItemID    string  `json:"itemId"`
	Reason    *string `json:"reason"`
	GrantRoot *string `json:"grantRoot"`
}

// PermissionsApproval asks for filesystem or network access beyond the
// sandbox. Granting echoes Permissions back; an empty object grants nothing.
type PermissionsApproval struct {
	ThreadID    string          `json:"threadId"`
	TurnID      string          `json:"turnId"`
	ItemID      string          `json:"itemId"`
	Cwd         string          `json:"cwd"`
	Reason      *string         `json:"reason"`
	Permissions json.RawMessage `json:"permissions"`
}

// PermissionsResponse answers a PermissionsApproval; Scope is "turn" or "session".
type PermissionsResponse struct {
	Permissions json.RawMessage `json:"permissions"`
	Scope       string          `json:"scope,omitempty"`
}

// DecisionResponse answers a command or file-change approval: "accept",
// "acceptForSession", "decline", "cancel", or an object decision.
type DecisionResponse struct {
	Decision any `json:"decision"`
}

// UserInput is the model's request_user_input: questions answered by id.
type UserInput struct {
	ThreadID   string              `json:"threadId"`
	TurnID     string              `json:"turnId"`
	ItemID     string              `json:"itemId"`
	IsBlocking bool                `json:"isBlocking"`
	Questions  []UserInputQuestion `json:"questions"`
}

// UserInputQuestion allows free text when IsOther is set or it has no options.
type UserInputQuestion struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	Question string `json:"question"`
	IsOther  bool   `json:"isOther"`
	Options  []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

// UserInputResponse answers each question by id with the chosen labels and
// any typed text.
type UserInputResponse struct {
	Answers map[string]UserInputAnswer `json:"answers"`
}

type UserInputAnswer struct {
	Answers []string `json:"answers"`
}

// Elicitation is an MCP server's form or link, relayed by Codex. Mode is
// "form" (RequestedSchema) or another mode the host declines. A form with no
// fields is Codex asking to run an MCP tool, whose arguments are in Meta.
type Elicitation struct {
	ThreadID        string          `json:"threadId"`
	TurnID          *string         `json:"turnId"`
	ServerName      string          `json:"serverName"`
	Mode            string          `json:"mode"`
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema"`
	Meta            json.RawMessage `json:"_meta"`
}

// ElicitationResponse's Action is "accept", "decline" or "cancel".
type ElicitationResponse struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content,omitempty"`
}
