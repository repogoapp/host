package codexappserver

import (
	"context"
	"encoding/json"
)

// Initialize is the handshake: the server answers nothing else until the
// client has said it is initialized. Experimental fields carry plan mode.
func (c *Client) Initialize(ctx context.Context) error {
	if err := c.Call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "repogo", "title": "RepoGo", "version": "0.1.0"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, nil); err != nil {
		return err
	}
	return c.Notify(ctx, "initialized", nil)
}

// ThreadParams open a thread. Config is Codex's config.toml as an object,
// layered over the user's own; it carries the MCP servers a turn gets.
type ThreadParams struct {
	ThreadID string         `json:"threadId,omitempty"`
	Cwd      string         `json:"cwd,omitempty"`
	Config   map[string]any `json:"config,omitempty"`
}

// ThreadResponse is what thread/start and thread/resume answer with.
type ThreadResponse struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
	Model           string  `json:"model"`
	ReasoningEffort *string `json:"reasoningEffort"`
}

// ThreadStart opens a new thread; its id names the rollout file.
func (c *Client) ThreadStart(ctx context.Context, p ThreadParams) (ThreadResponse, error) {
	var out ThreadResponse
	err := c.Call(ctx, "thread/start", p, &out)
	return out, err
}

// ThreadResume reopens the thread p.ThreadID from its rollout.
func (c *Client) ThreadResume(ctx context.Context, p ThreadParams) (ThreadResponse, error) {
	var out ThreadResponse
	err := c.Call(ctx, "thread/resume", p, &out)
	return out, err
}

// Input is one part of a prompt: "text", "localImage" (a path) or
// "mention" (a file by name and path).
type Input struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"`
}

// SandboxPolicy is where commands may write; Type is readOnly,
// workspaceWrite or dangerFullAccess.
type SandboxPolicy struct {
	Type string `json:"type"`
}

// CollaborationMode is "plan" or "default". It overrides the model and
// effort, so Settings repeats the ones the turn runs with.
type CollaborationMode struct {
	Mode     string `json:"mode"`
	Settings struct {
		Model           string  `json:"model"`
		ReasoningEffort *string `json:"reasoning_effort"`
	} `json:"settings"`
}

// TurnParams start a turn. Every setting sticks for later turns of the
// thread, so an empty one keeps what the last turn ran with.
type TurnParams struct {
	ThreadID           string             `json:"threadId"`
	Input              []Input            `json:"input"`
	Model              string             `json:"model,omitempty"`
	Effort             string             `json:"effort,omitempty"`
	ApprovalPolicy     string             `json:"approvalPolicy,omitempty"`
	SandboxPolicy      *SandboxPolicy     `json:"sandboxPolicy,omitempty"`
	ServiceTierForTurn string             `json:"serviceTierForTurn,omitempty"`
	CollaborationMode  *CollaborationMode `json:"collaborationMode,omitempty"`
}

// Turn is a turn as Codex reports it. Status is inProgress, completed,
// interrupted or failed; Error is set only when it failed.
type Turn struct {
	ID     string     `json:"id"`
	Status string     `json:"status"`
	Error  *TurnError `json:"error"`
}

type TurnError struct {
	Message           string  `json:"message"`
	AdditionalDetails *string `json:"additionalDetails"`
}

// TurnStart starts a turn and answers at once; it ends with turn/completed.
func (c *Client) TurnStart(ctx context.Context, p TurnParams) (Turn, error) {
	var out struct {
		Turn Turn `json:"turn"`
	}
	err := c.Call(ctx, "turn/start", p, &out)
	return out.Turn, err
}

// SteerParams add input to the running turn ExpectedTurnID. Codex tags the
// userMessage item it writes with ClientUserMessageID.
type SteerParams struct {
	ThreadID            string  `json:"threadId"`
	ExpectedTurnID      string  `json:"expectedTurnId"`
	Input               []Input `json:"input"`
	ClientUserMessageID string  `json:"clientUserMessageId,omitempty"`
}

// TurnSteer adds input the turn reads before its next model request. Every
// refusal (no active turn, plan mode, another turn, review) is an *Error.
func (c *Client) TurnSteer(ctx context.Context, p SteerParams) (string, error) {
	var out struct {
		TurnID string `json:"turnId"`
	}
	err := c.Call(ctx, "turn/steer", p, &out)
	return out.TurnID, err
}

// TurnInterrupt stops a running turn, which then completes as interrupted.
func (c *Client) TurnInterrupt(ctx context.Context, threadID, turnID string) error {
	return c.Call(ctx, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": turnID}, nil)
}

// Raw calls a method whose result the caller reads itself.
func (c *Client) Raw(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.Call(ctx, method, params, &out)
	return out, err
}
