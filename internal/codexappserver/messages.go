package codexappserver

import (
	"encoding/json"

	"github.com/repogo/host/internal/stdiorpc"
)

// Notification is one message Codex streams without expecting an answer.
type Notification = stdiorpc.Notification

// The notifications a turn is read from.
const (
	MethodTurnStarted           = "turn/started"
	MethodTurnCompleted         = "turn/completed"
	MethodItemStarted           = "item/started"
	MethodItemCompleted         = "item/completed"
	MethodAgentMessageDelta     = "item/agentMessage/delta"
	MethodReasoningSummaryDelta = "item/reasoning/summaryTextDelta"
	MethodReasoningTextDelta    = "item/reasoning/textDelta"
	MethodTokenUsageUpdated     = "thread/tokenUsage/updated"
	MethodError                 = "error"
	MethodServerRequestResolved = "serverRequest/resolved"
)

// TurnNotification is turn/started and turn/completed.
type TurnNotification struct {
	ThreadID string `json:"threadId"`
	Turn     Turn   `json:"turn"`
}

// ItemNotification is item/started and item/completed.
type ItemNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Item     Item   `json:"item"`
}

// Item is one thread item. Type says which fields are set: agentMessage
// (Text), reasoning, commandExecution, fileChange, mcpToolCall,
// dynamicToolCall, collabAgentToolCall, webSearch, imageView and others.
type Item struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`

	Text string `json:"text"`
	// Delivery is "async" on the message Codex writes for request_user_input_async:
	// the question as plain text, for clients without a question card.
	Delivery string `json:"delivery"`

	Command          string          `json:"command"`
	Cwd              string          `json:"cwd"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	Changes          json.RawMessage `json:"changes"`

	Server       string          `json:"server"`
	Namespace    string          `json:"namespace"`
	Tool         string          `json:"tool"`
	Arguments    json.RawMessage `json:"arguments"`
	Result       json.RawMessage `json:"result"`
	ContentItems json.RawMessage `json:"contentItems"`
	Error        *struct {
		Message string `json:"message"`
	} `json:"error"`

	Query  string          `json:"query"`
	Action json.RawMessage `json:"action"`
	Path   string          `json:"path"`
}

// DeltaNotification is a streamed piece of an item's text.
type DeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

// TokenUsageNotification reports the thread's running token counts after
// each model request.
type TokenUsageNotification struct {
	ThreadID   string `json:"threadId"`
	TurnID     string `json:"turnId"`
	TokenUsage struct {
		Last               TokenBreakdown `json:"last"`
		Total              TokenBreakdown `json:"total"`
		ModelContextWindow *int64         `json:"modelContextWindow"`
	} `json:"tokenUsage"`
}

// TokenBreakdown counts one span. InputTokens includes the cached ones.
type TokenBreakdown struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

// ErrorNotification is a failed model request; WillRetry says Codex is
// trying again, so the turn goes on.
type ErrorNotification struct {
	ThreadID  string    `json:"threadId"`
	TurnID    string    `json:"turnId"`
	Error     TurnError `json:"error"`
	WillRetry bool      `json:"willRetry"`
}

// ResolvedNotification says Codex settled one of its own requests.
type ResolvedNotification struct {
	ThreadID  string          `json:"threadId"`
	RequestID json.RawMessage `json:"requestId"`
}
