package antigravity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/notify"
)

// agy reads hooks from ~/.gemini/config/hooks.json, an object of named hooks,
// each mapping its events to handlers: tool events in matcher groups, the
// rest as plain lists. RepoGo's is the one named repogo. Its handlers print
// nothing, which agy reads as letting a tool run.

// hookName is RepoGo's entry in hooks.json.
const hookName = "repogo"

// hookEvents are the events whose status the host reads; agy has none for an
// approval prompt, so a chat waiting on one still shows as working.
var hookEvents = []string{"SessionStart", "PreInvocation", "PreToolUse", "PostToolUse", "Stop"}

// toolEvents take their handlers in a matcher group.
var toolEvents = map[string]bool{"PreToolUse": true, "PostToolUse": true}

// hookTimeout caps a handler, in seconds; the helper is a few shell builtins.
const hookTimeout = 5

// Hooks edits agy's hooks.json in Dir. Home is agy's own folder: without it
// agy isn't here, and no hooks file is made for it.
type Hooks struct{ Dir, Home string }

func (Hooks) Kind() agent.Kind { return agent.KindAntigravity }

func (h Hooks) path() string { return filepath.Join(h.Dir, "hooks.json") }

// Hooks wires the helper into agy's hooks.json.
func (p *Provider) Hooks() notify.Provider { return Hooks{Dir: p.configDir, Home: p.home} }

// command runs the helper for one event, except in a chat RepoGo runs: the
// host follows those itself, under its own id for the chat.
func command(helper, event string) string {
	return fmt.Sprintf(`[ "$%s" = %s ] || '%s' %s %s`, notify.OriginEnv, notify.OriginApp,
		strings.ReplaceAll(helper, "'", `'\''`), agent.KindAntigravity, event)
}

// entry is RepoGo's named hook for helper.
func entry(helper string) map[string]any {
	out := map[string]any{}
	for _, event := range hookEvents {
		handler := map[string]any{"type": "command", "command": command(helper, event), "timeout": hookTimeout}
		if toolEvents[event] {
			out[event] = []any{map[string]any{"matcher": "*", "hooks": []any{handler}}}
		} else {
			out[event] = []any{handler}
		}
	}
	return out
}

// read is hooks.json's named hooks, each left as written; none when the file
// doesn't exist. One that isn't JSON is an error, so it is never overwritten.
func (h Hooks) read() (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(h.path())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	all := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &all); err != nil {
			return nil, fmt.Errorf("%s isn't JSON: %w", h.path(), err)
		}
	}
	return all, nil
}

// Status reads hooks.json without changing it: installed when RepoGo's entry
// runs this helper for every event, a conflict when an entry of that name
// is someone else's.
func (h Hooks) Status() notify.AgentStatus {
	st := notify.AgentStatus{Agent: string(agent.KindAntigravity), ConfigPath: h.path()}
	all, err := h.read()
	if err != nil {
		st.Conflict = err.Error()
		return st
	}
	ours, ok := all[hookName]
	if !ok {
		return st
	}
	if !strings.Contains(string(ours), helperName) {
		st.Conflict = "hooks.json has a hook named " + hookName + " that isn't RepoGo's"
		return st
	}
	var events map[string]json.RawMessage
	_ = json.Unmarshal(ours, &events)
	st.Installed = true
	for _, event := range hookEvents {
		if !strings.Contains(string(events[event]), helperName) {
			st.Installed = false
		}
	}
	return st
}

// helperName is the helper's file name, in every handler RepoGo writes.
const helperName = "repogo-notify"

// Ensure writes RepoGo's entry, leaving every other hook as it was; on a
// machine where agy has never run it does nothing.
func (h Hooks) Ensure(helper string) (notify.AgentStatus, error) {
	if st := h.Status(); st.Installed || st.Conflict != "" {
		return st, nil
	}
	if _, err := os.Stat(h.Home); err != nil {
		return h.Status(), nil
	}
	all, err := h.read()
	if err != nil {
		return notify.AgentStatus{}, err
	}
	ours, err := json.Marshal(entry(helper))
	if err != nil {
		return notify.AgentStatus{}, err
	}
	all[hookName] = ours
	if err := h.write(all); err != nil {
		return notify.AgentStatus{}, err
	}
	return h.Status(), nil
}

// Remove takes RepoGo's entry out of hooks.json, and the file with it when
// nothing else is left in it.
func (h Hooks) Remove(string) error {
	all, err := h.read()
	if err != nil {
		return err
	}
	if _, ok := all[hookName]; !ok {
		return nil
	}
	delete(all, hookName)
	if len(all) == 0 {
		return os.Remove(h.path())
	}
	return h.write(all)
}

// write replaces hooks.json in one rename, so agy never reads half a file.
func (h Hooks) write(all map[string]json.RawMessage) error {
	raw, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(h.Dir, ".hooks-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), h.path())
}

// HookStatus maps an agy hook event to a chat status. A Stop's reason comes as
// notification: an error fails the turn, a cancel interrupts it.
func (Hooks) HookStatus(event, notification, tool string) agent.ChatStatus {
	switch event {
	case "SessionStart", "PreInvocation", "PostToolUse":
		return agent.ChatWorking
	case "PreToolUse":
		if tool == "ask_question" {
			return agent.ChatAwaitingUser
		}
		return agent.ChatWorking
	case "Stop":
		switch notification {
		case "ERROR":
			return agent.ChatFailed
		case "USER_CANCELED":
			return agent.ChatInterrupted
		}
		return agent.ChatCompleted
	}
	return agent.ChatUnknown
}

// Displays is false: agy has no hook that streams a turn's text.
func (Hooks) Displays() bool { return false }
