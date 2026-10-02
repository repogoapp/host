package codex

import (
	"path/filepath"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/notify"
)

// codexEvents is every event whose status the host reads. A prompt hook
// names the model before the rollout does, so a new chat's row has it.
var codexEvents = []string{
	"UserPromptSubmit", "PreToolUse", "PermissionRequest", "PostToolUse", "Stop", "Interrupt",
}

// codexTimeouts caps each hook at Codex's call, in seconds; the helper is a
// few shell builtins, so a slow one is hung and must not hold up a turn.
var codexTimeouts = map[string]int{
	"UserPromptSubmit": 5, "PreToolUse": 5, "PermissionRequest": 5, "PostToolUse": 5, "Stop": 5, "Interrupt": 5,
}

// Hooks edits Codex's user hooks.json. Codex runs a hook only once the user
// has trusted it (`/hooks` in the TUI), so an install reports nothing until then.
type Hooks struct{ Home string }

// Kind is the agent name the helper is registered under.
func (Hooks) Kind() agent.Kind { return agent.KindCodex }

func (c Hooks) file() notify.HookFile {
	return notify.HookFile{
		Path: filepath.Join(c.Home, "hooks.json"), Agent: string(agent.KindCodex),
		Events: codexEvents, Timeouts: codexTimeouts,
	}
}

// Status reads hooks.json without modifying it.
func (c Hooks) Status() notify.AgentStatus { return notify.FileStatus(agent.KindCodex, c.file()) }

// Ensure appends one handler group per missing event; see notify.HookFile.
func (c Hooks) Ensure(helper string) (notify.AgentStatus, error) {
	if err := c.file().Ensure(helper); err != nil {
		return notify.AgentStatus{}, err
	}
	return c.Status(), nil
}

// Remove takes the helper's handlers out of hooks.json.
func (c Hooks) Remove(helper string) error { return c.file().Remove(helper) }

// HookStatus maps a Codex hook event to a chat status.
func (Hooks) HookStatus(event, notification, tool string) agent.ChatStatus {
	switch event {
	case "UserPromptSubmit", "PreToolUse", "PostToolUse":
		return agent.ChatWorking
	case "PermissionRequest":
		return agent.ChatAwaitingApproval
	case "Stop":
		return agent.ChatCompleted
	case "Interrupt":
		return agent.ChatInterrupted
	}
	return agent.ChatUnknown
}

// Displays is false: Codex has no hook that streams a turn's text.
func (Hooks) Displays() bool { return false }
