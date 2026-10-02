package agent

import "slices"

// ChatStatus describes the latest observed activity, not whether a process exists.
type ChatStatus string

const (
	ChatUnknown          ChatStatus = "unknown"
	ChatIdle             ChatStatus = "idle"
	ChatQueued           ChatStatus = "queued"
	ChatWorking          ChatStatus = "working"
	ChatAwaitingApproval ChatStatus = "awaiting_approval"
	ChatAwaitingUser     ChatStatus = "awaiting_user"
	ChatCompleted        ChatStatus = "completed"
	ChatFailed           ChatStatus = "failed"
	ChatCancelled        ChatStatus = "cancelled"
	ChatInterrupted      ChatStatus = "interrupted"
)

// ActiveStatuses are a turn queued, running, or blocked on the user.
var ActiveStatuses = []ChatStatus{ChatQueued, ChatWorking, ChatAwaitingApproval, ChatAwaitingUser}

// Active reports whether s is one of ActiveStatuses.
func (s ChatStatus) Active() bool { return slices.Contains(ActiveStatuses, s) }

func (s ChatStatus) Valid() bool {
	switch s {
	case ChatUnknown, ChatIdle, ChatQueued, ChatWorking, ChatAwaitingApproval,
		ChatAwaitingUser, ChatCompleted, ChatFailed, ChatCancelled, ChatInterrupted:
		return true
	}
	return false
}

// AtRest is the status a chat row settles to once a turn with this outcome is
// over: a completed turn leaves the chat idle, while a failure stays visible.
func (s ChatStatus) AtRest() ChatStatus {
	if s == ChatCompleted {
		return ChatIdle
	}
	return s
}
