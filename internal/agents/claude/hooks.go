package claude

import (
	"path/filepath"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/notify"
)

// claudeEvents is every lifecycle event the helper is registered for.
var claudeEvents = []string{
	"SessionStart", "SessionEnd", "UserPromptSubmit", "Stop", "StopFailure",
	"Notification", "PermissionRequest", "SubagentStart", "SubagentStop",
	"PreToolUse", "PostToolUse", "PostToolUseFailure",
	"MessageDisplay",
}

// claudeTimeouts overrides Claude's hook timeout per event, in seconds. Claude
// blocks rendering on MessageDisplay, so a hung helper would freeze the user's
// terminal; the helper is a few shell builtins, so two seconds is generous.
var claudeTimeouts = map[string]int{"MessageDisplay": 2}

// Hooks edits the `hooks` key of Claude's user settings.json.
type Hooks struct{ Home string }

// Kind is the agent name the helper is registered under.
func (Hooks) Kind() agent.Kind { return agent.KindClaude }

func (c Hooks) file() notify.HookFile {
	return notify.HookFile{
		Path: filepath.Join(c.Home, "settings.json"), Agent: string(agent.KindClaude),
		Events: claudeEvents, Matcher: "*", Timeouts: claudeTimeouts,
	}
}

// Status reads settings.json without modifying it.
func (c Hooks) Status() notify.AgentStatus { return notify.FileStatus(agent.KindClaude, c.file()) }

// Ensure appends one handler group per missing event; see notify.HookFile.
func (c Hooks) Ensure(helper string) (notify.AgentStatus, error) {
	if err := c.file().Ensure(helper); err != nil {
		return notify.AgentStatus{}, err
	}
	return c.Status(), nil
}

// Remove takes the helper's handlers out of settings.json.
func (c Hooks) Remove(helper string) error { return c.file().Remove(helper) }

// HookStatus maps a Claude hook event to a chat status. Only explicit lifecycle
// signals change status; unrelated notifications are ignored.
func (Hooks) HookStatus(event, notification, tool string) agent.ChatStatus {
	switch event {
	case "Stop":
		return agent.ChatCompleted
	case "StopFailure":
		return agent.ChatFailed
	case "PermissionRequest":
		if tool == "AskUserQuestion" {
			return agent.ChatAwaitingUser
		}
		return agent.ChatAwaitingApproval
	case "PreToolUse":
		if tool == "AskUserQuestion" {
			return agent.ChatAwaitingUser
		}
		return agent.ChatWorking
	case "UserPromptSubmit", "PostToolUse", "PostToolUseFailure":
		return agent.ChatWorking
	case "Notification":
		switch notification {
		case "permission_prompt":
			return agent.ChatAwaitingApproval
		case "elicitation_dialog":
			return agent.ChatAwaitingUser
		case "idle_prompt":
			return agent.ChatIdle
		}
	}
	return agent.ChatUnknown
}

// Displays is true: Claude streams a terminal turn's text through MessageDisplay.
func (Hooks) Displays() bool { return true }
