package notify

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/repogo/host/internal/apphome"
)

// HookFile is an agent's JSON hook config: `{"hooks": {Event: [group…]}}`,
// where a group is `{"matcher", "hooks": [handler…]}`. Claude's settings.json
// and Codex's hooks.json share the shape.
type HookFile struct {
	Path  string
	Agent string
	// Events is every event the helper is registered for.
	Events []string
	// Matcher is written on each group we add; "" leaves it out.
	Matcher string
	// Timeouts overrides the agent's hook timeout per event, in seconds.
	Timeouts map[string]int
}

// Command is the exact registered command; status checks match on the helper.
func (f HookFile) Command(helper, event string) string {
	return fmt.Sprintf("%q %s %s", helper, f.Agent, event)
}

// Ensure appends one group per event missing the helper. Everything else in
// the file is carried through as raw JSON, so other tools' hooks, the user's
// matchers and every other key are written back as they were.
func (f HookFile) Ensure(helper string) error {
	doc, hooks, err := f.read()
	if err != nil {
		return err
	}
	added := 0
	for _, event := range f.Events {
		if groupsInvoke(hooks[event], helper) {
			continue
		}
		handler := map[string]any{"type": "command", "command": f.Command(helper, event)}
		if timeout, ok := f.Timeouts[event]; ok {
			handler["timeout"] = timeout
		}
		group := map[string]any{"hooks": []any{handler}}
		if f.Matcher != "" {
			group["matcher"] = f.Matcher
		}
		raw, err := json.Marshal(group)
		if err != nil {
			return err
		}
		hooks[event] = append(hooks[event], raw)
		added++
	}
	if added == 0 {
		return nil
	}
	return f.write(doc, hooks)
}

// Remove takes out every handler that invokes the helper, then any group and
// event it leaves empty. Handlers sharing a group with ours stay.
func (f HookFile) Remove(helper string) error {
	if _, err := os.Stat(f.Path); os.IsNotExist(err) {
		return nil
	}
	doc, hooks, err := f.read()
	if err != nil {
		return err
	}
	removed := 0
	for event, groups := range hooks {
		kept := groups[:0:0]
		for _, raw := range groups {
			group, n, err := withoutHelper(raw, helper)
			if err != nil {
				return fmt.Errorf("parse %s %s hooks: %w", f.Path, event, err)
			}
			removed += n
			if group != nil {
				kept = append(kept, group)
			}
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if removed == 0 {
		return nil
	}
	return f.write(doc, hooks)
}

// Installed reports whether every event's groups invoke the helper.
func (f HookFile) Installed(helper string) bool {
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return false
	}
	var doc struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	for _, event := range f.Events {
		if !groupsInvoke(doc.Hooks[event], helper) {
			return false
		}
	}
	return true
}

// read parses the file into its top-level keys and its hooks by event. A file
// that does not exist reads as empty; one that does not parse is an error, so
// we never replace a file we failed to understand.
func (f HookFile) read() (map[string]json.RawMessage, map[string][]json.RawMessage, error) {
	doc := map[string]json.RawMessage{}
	hooks := map[string][]json.RawMessage{}
	raw, err := os.ReadFile(f.Path)
	if os.IsNotExist(err) {
		return doc, hooks, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", f.Path, err)
	}
	if doc == nil {
		return nil, nil, fmt.Errorf("parse %s: must be a JSON object", f.Path)
	}
	if v, ok := doc["hooks"]; ok && strings.TrimSpace(string(v)) != "null" {
		if err := json.Unmarshal(v, &hooks); err != nil {
			return nil, nil, fmt.Errorf("parse %s hooks: %w", f.Path, err)
		}
	}
	return doc, hooks, nil
}

// write saves hooks back under doc, keeping the mode the user gave the file.
func (f HookFile) write(doc map[string]json.RawMessage, hooks map[string][]json.RawMessage) error {
	raw, err := json.Marshal(hooks)
	if err != nil {
		return err
	}
	doc["hooks"] = raw
	perm := os.FileMode(0o600)
	if info, err := os.Stat(f.Path); err == nil {
		perm = info.Mode().Perm()
	}
	return apphome.WriteJSON(f.Path, doc, perm)
}

// groupsInvoke reports whether any handler in groups invokes the helper.
// Matching on the path, not the command, keeps an entry with different
// quoting or arguments from being added twice.
func groupsInvoke(groups []json.RawMessage, helper string) bool {
	for _, g := range groups {
		if strings.Contains(string(g), helper) {
			return true
		}
	}
	return false
}

// withoutHelper drops the group's handlers that invoke the helper and reports
// how many went. The group comes back nil once it has no handlers left, and
// unchanged, byte for byte, when none were ours.
func withoutHelper(raw json.RawMessage, helper string) (json.RawMessage, int, error) {
	if !strings.Contains(string(raw), helper) {
		return raw, 0, nil
	}
	var group map[string]json.RawMessage
	if err := json.Unmarshal(raw, &group); err != nil {
		return nil, 0, err
	}
	var handlers []json.RawMessage
	if err := json.Unmarshal(group["hooks"], &handlers); err != nil {
		return nil, 0, err
	}
	kept := handlers[:0:0]
	for _, h := range handlers {
		if !strings.Contains(string(h), helper) {
			kept = append(kept, h)
		}
	}
	removed := len(handlers) - len(kept)
	if len(kept) == 0 {
		return nil, removed, nil
	}
	var err error
	if group["hooks"], err = json.Marshal(kept); err != nil {
		return nil, 0, err
	}
	out, err := json.Marshal(group)
	return out, removed, err
}
