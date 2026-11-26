package agent

import "slices"

// ChatStatus is where a chat's latest turn stands: queued, working or waiting
// on the user while it runs, then how it ended (completed, failed, cancelled,
// interrupted), kept until the next turn starts. Unknown is a chat the host
// has no word on. It describes the turn, not whether a process exists.
type ChatStatus string

const (
	ChatUnknown ChatStatus = "unknown"
	// ChatIdle is no longer written: older hosts saved a completed turn as
	// idle. Rows and apps may still hold it; it reads as at rest, no outcome.
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
