// Package agent runs turns over any Adapter and normalizes what they emit, and
// holds the shared CLI mechanics (install, update, sign-in, one-shots) that
// each provider in internal/agents configures.
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/repogo/host/internal/device"
)

// Kind names a Provider; the constants live beside each provider's entry.
type Kind string

// ChatID is how a chat is named everywhere: "<agent>:<session_id>".
func ChatID(kind Kind, sessionID string) string { return string(kind) + ":" + sessionID }

// SplitChatID undoes ChatID; ok is false unless both halves are present.
func SplitChatID(id string) (kind Kind, sessionID string, ok bool) {
	k, sessionID, ok := strings.Cut(id, ":")
	return Kind(k), sessionID, ok && k != "" && sessionID != ""
}

// Mode is one operating mode an agent reported for a session, in the agent's
// own words; each CLI names its own.
type Mode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ConfigOption is one knob an agent reported for a session. Reported, never
// hardcoded: the model list changes with every CLI release.
type ConfigOption struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Description  string        `json:"description,omitempty"`
	CurrentValue string        `json:"current_value,omitempty"`
	Values       []ConfigValue `json:"values"`
}

type ConfigValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// PermissionMode is how aggressively an agent may act without asking. Each
// adapter maps it onto its own CLI's vocabulary; the names here are the
// product's, not any provider's.
type PermissionMode string

const (
	PermissionApprovalRequired PermissionMode = "approval-required"
	PermissionAutoAcceptEdits  PermissionMode = "auto-accept-edits"
	PermissionFullAccess       PermissionMode = "full-access"
)

// TurnConfig is the flat per-turn knob set. Empty means "use the agent's
// default" — never a zero value the adapter has to guess about.
type TurnConfig struct {
	PermissionMode PermissionMode `json:"permission_mode,omitempty"`

	// agent | plan. Claude maps plan onto its own permission mode; Codex has
	// no planning mode and ignores it.
	Mode string `json:"mode,omitempty"`

	Model string `json:"model,omitempty"`

	// low | medium | high | xhigh | max. Claude passes it as --effort; Codex as
	// a model_reasoning_effort config override.
	ReasoningLevel string `json:"reasoning_level,omitempty"`

	// Claude only, as --autocompact: "auto" or a token count.
	ContextWindow string `json:"context_window,omitempty"`

	// Faster output for more quota, on the models that offer it. Both bridges
	// expose it as an on/off option; a model without it ignores it.
	FastMode bool `json:"fast_mode,omitempty"`
}

type TurnRequest struct {
	// Serializes turns. Two sends against the same ChatID queue; different
	// chats run concurrently.
	ChatID string `json:"chat_id"`

	Cwd    string     `json:"cwd"`
	Agent  Kind       `json:"agent"`
	Prompt string     `json:"prompt"`
	Config TurnConfig `json:"config,omitempty"`

	// Title names a new chat; the device writes it from the first prompt.
	// Empty leaves the chat to the prompt's opening words.
	Title string `json:"title,omitempty"`

	// Provider-side conversation to continue. Empty starts fresh. This is the
	// provider's own id, not ours — we do not reimplement conversation state
	// the CLI already keeps.
	SessionID string `json:"session_id,omitempty"`

	// Files sent with the prompt, already on this host's disk. A prompt may
	// be nothing but attachments: a screenshot is a whole question.
	Attachments []Attachment `json:"attachments,omitempty"`

	// Device sent the turn, taken from its connection and never from params:
	// RepoGo's browser tool drives the preview on that phone.
	Device device.ID `json:"-"`
}

// Attachment is one file that came with a prompt. The bytes are read back
// from Path when the turn runs, so the request stays small enough to report.
type Attachment struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Path     string `json:"path"`
}

func (a Attachment) IsImage() bool { return strings.HasPrefix(a.MimeType, "image/") }

type EventKind string

const (
	EventTurnStarted EventKind = "turn_started"

	// What the user said. Absent from a turn we start (the caller already has
	// the prompt) but present throughout a transcript we read back, which is
	// what lets one Event type describe both.
	EventUserMessage EventKind = "user_message"

	EventText         EventKind = "text"
	EventReasoning    EventKind = "reasoning"
	EventToolCall     EventKind = "tool_call"
	EventToolResult   EventKind = "tool_result"
	EventTurnFinished EventKind = "turn_finished"
	EventTurnFailed   EventKind = "turn_failed"

	// The agent is blocked until someone answers. This is the state that is
	// invisible on disk, so only a live protocol client can report it.
	EventApprovalRequested EventKind = "approval_requested"
	EventApprovalResolved  EventKind = "approval_resolved"
)

// Approval is an agent blocked on a person: permission to act, answered with
// one of Options (the agent's own, echoed back verbatim), or Questions,
// answered per question.
type Approval struct {
	CallID    string           `json:"call_id"`
	Title     string           `json:"title"`
	Kind      string           `json:"kind"`
	Input     json.RawMessage  `json:"input,omitempty"`
	Options   []ApprovalOption `json:"options,omitempty"`
	Questions []Question       `json:"questions,omitempty"`
}

// Question is one field of an agent's form. The answer is keyed by ID and is
// an option label, or labels when MultiSelect; free text goes under CustomID.
type Question struct {
	ID          string           `json:"id"`
	CustomID    string           `json:"custom_id,omitempty"`
	Header      string           `json:"header,omitempty"`
	Text        string           `json:"text"`
	MultiSelect bool             `json:"multi_select,omitempty"`
	Options     []QuestionOption `json:"options"`
}

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ApprovalQuestion is the Kind of an approval that is a form to fill in.
const ApprovalQuestion = "question"

type ApprovalOption struct {
	OptionID string `json:"option_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type ToolCall struct {
	CallID  string          `json:"call_id,omitempty"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

type Usage struct {
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	DurationMS          int64   `json:"duration_ms"`

	// ContextUsed is how full the window was for this turn's last request and
	// ContextSize the window itself; zero is unknown. A window, not a total, so not
	// derived from the token counts; not persisted, the transcript is durable.
	ContextUsed int64 `json:"context_used,omitempty"`
	ContextSize int64 `json:"context_size,omitempty"`
}

// Event is the normalized unit. Seq is monotonic within a turn and is assigned
// by the Manager, not by adapters — an adapter that counted its own would drift
// the moment it emitted from more than one place.
type Event struct {
	TurnID string    `json:"turn_id"`
	Seq    uint64    `json:"seq"`
	Kind   EventKind `json:"kind"`

	// When it happened, Unix milliseconds. Stamped by the Manager on a live
	// turn and by the parser from the transcript line on a read-back, so a
	// client can time a turn either way. Zero is unknown.
	At int64 `json:"at,omitempty"`

	// Deltas for EventText and EventReasoning. Consumers append; they are never
	// cumulative.
	Text string `json:"text,omitempty"`

	Tool *ToolCall `json:"tool,omitempty"`

	// Set on EventTurnStarted and EventTurnFinished so a caller can resume this
	// conversation later without tracking it separately.
	SessionID string `json:"session_id,omitempty"`

	// Set on EventApprovalRequested.
	Approval *Approval `json:"approval,omitempty"`

	Usage *Usage `json:"usage,omitempty"`
	Error string `json:"error,omitempty"`
}

// TurnIO is what an adapter is given to talk to the outside world. Ask is
// separate from Emit because it blocks, which is what a permission prompt needs.
type TurnIO struct {
	Emit func(Event)

	// Activity keeps a turn alive while the provider owns background work.
	Activity func(busy bool)

	// Session reports the provider's conversation id as soon as the session opens,
	// before the prompt runs: a chat's identity is `agent:<session id>` and a
	// client has nothing to open until this fires. Optional, called at most once.
	Session func(sessionID string)

	// Ask blocks until the user answers or ctx ends: a JSON string naming the
	// option, or an object of answers for a form. Returning an error declines.
	Ask func(context.Context, Approval) (json.RawMessage, error)

	// The turn and who sent it, for RepoGo's own tools.
	TurnID string
	Device device.ID
}

// Adapter is one coding agent. Implementations own their CLI's protocol and
// nothing else — no queueing, no sequencing, no fan-out.
type Adapter interface {
	Kind() Kind

	// Available reports whether this agent can run here: binary present and
	// logged in. Checked before a turn is accepted so a caller gets a clear
	// error instead of a failed turn.
	Available() error

	// Send runs one turn, calling io.Emit for each event, and returns when it is
	// over. Cancelling ctx is stop; answers arrive through io.Ask. It must not
	// emit EventTurnStarted/Finished/Failed; the Manager owns lifecycle.
	Send(ctx context.Context, req TurnRequest, io TurnIO) (Result, error)
}

// Steerer is an Adapter that adds input to its running turn turnID, read
// before the model's next request. An error means the input was not added.
type Steerer interface {
	Steer(ctx context.Context, turnID string, req TurnRequest) error
}

// Interrupter is an Adapter whose process for a chat can start a turn of its
// own between the Manager's turns, such as a reply to a finished background
// task. Interrupt stops that turn and reports whether one was running.
type Interrupter interface {
	Interrupt(ctx context.Context, chatID string) (bool, error)
}

// Result is what an adapter learned about the turn as a whole.
type Result struct {
	SessionID  string
	StopReason string
	Usage      *Usage
}

// UnixMillis is an RFC 3339 timestamp, as every provider writes them, in unix
// milliseconds; 0 when empty or unreadable, which clients read as unknown.
func UnixMillis(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}
