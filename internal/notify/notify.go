// Package notify bridges agent hooks into the event layer, because a transcript
// goes silent both when waiting for approval and when finished. The hook is a
// POSIX helper writing a drop file, which survives the runtime being down.
package notify

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
)

// Notice is one hook firing.
type Notice struct {
	Agent     agent.Kind `json:"agent"`
	SessionID string     `json:"session_id"`
	// OriginApp for an agent this host spawned for a device, OriginCLI for the
	// user's terminal; the helper reads it from the environment set at spawn.
	Origin     Origin           `json:"origin"`
	PromptID   string           `json:"prompt_id,omitempty"`
	TurnID     string           `json:"turn_id,omitempty"`
	StopReason *string          `json:"stop_reason,omitempty"`
	Error      string           `json:"error,omitempty"`
	Status     agent.ChatStatus `json:"status"`
	Message    string           `json:"message,omitempty"`
	Cwd        string           `json:"cwd,omitempty"`
	Event      string           `json:"event,omitempty"`
	At         time.Time        `json:"at"`

	// What the hook was about, kept off the wire and out of turn history.
	// Questions is the tool input of a question put to the user, decoded.
	// Model is what the turn runs on, where the agent's hook names it.
	Model     string           `json:"-"`
	Prompt    string           `json:"-"`
	ToolName  string           `json:"-"`
	ToolUseID string           `json:"-"`
	ToolInput json.RawMessage  `json:"-"`
	Questions []agent.Question `json:"-"`
}

// ChatID is the chat the hook fired in.
func (n Notice) ChatID() string { return agent.ChatID(n.Agent, n.SessionID) }

// Turn names the turn the hook fired in, by the prompt id when the hook has
// one and the provider's turn id otherwise; source says which, since the two
// id spaces differ. Empty when the hook names no turn.
func (n Notice) Turn() (source, id string) {
	switch {
	case n.PromptID != "":
		return "hook-prompt", n.PromptID
	case n.TurnID != "":
		return "hook-turn", n.TurnID
	}
	return "", ""
}

// Starts reports a hook that opens a turn.
func (n Notice) Starts() bool { return n.Event == "UserPromptSubmit" }

// Ends reports a hook that closes a turn, whatever its outcome.
func (n Notice) Ends() bool {
	return n.Event == "Stop" || n.Event == "StopFailure" || n.Event == "Interrupt"
}

// Failed reports a turn that ended in failure.
func (n Notice) Failed() bool { return n.Event == "StopFailure" || n.Status == agent.ChatFailed }

// ToolStarting reports a hook fired before a tool runs, questions included.
func (n Notice) ToolStarting() bool { return n.Event == "PreToolUse" }

// ToolFinished reports a hook fired after a tool ran, whether or not it failed.
func (n Notice) ToolFinished() bool {
	return n.Event == "PostToolUse" || n.Event == "PostToolUseFailure"
}

// ToolFailed reports a tool that ran and failed.
func (n Notice) ToolFailed() bool { return n.Event == "PostToolUseFailure" }

// AsksQuestion reports the agent putting a question to the user; PreToolUse
// always fires for one, its PermissionRequest may not.
func (n Notice) AsksQuestion() bool { return n.ToolStarting() && n.ToolName == questionTool }

// AsksPermission reports a permission prompt for a tool other than a question.
func (n Notice) AsksPermission() bool {
	return n.Event == "PermissionRequest" && n.ToolName != questionTool
}

// Question is the first question asked, or "".
func (n Notice) Question() string {
	if len(n.Questions) == 0 {
		return ""
	}
	return n.Questions[0].Text
}

// questionTool is the tool through which Claude asks the user a question.
const questionTool = "AskUserQuestion"

// Questions reads a question tool's input into the form an app turn's
// question carries, so both read the same on the phone.
func Questions(raw json.RawMessage) []agent.Question {
	var in struct {
		Questions []struct {
			Question    string                 `json:"question"`
			Header      string                 `json:"header"`
			MultiSelect bool                   `json:"multiSelect"`
			Options     []agent.QuestionOption `json:"options"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(raw, &in)
	out := make([]agent.Question, 0, len(in.Questions))
	for i, q := range in.Questions {
		if text := strings.TrimSpace(q.Question); text != "" {
			out = append(out, agent.Question{ID: strconv.Itoa(i), Header: q.Header,
				Text: text, MultiSelect: q.MultiSelect, Options: q.Options})
		}
	}
	return out
}

// Origin is who submitted a prompt.
type Origin string

const (
	OriginCLI Origin = "cli"
	OriginApp Origin = "app"
)

// OriginEnv is the variable the host sets on agents it spawns, so hooks
// firing inside them can say the prompt was the app's.
const OriginEnv = "REPOGO_ORIGIN"

const pollInterval = 300 * time.Millisecond

// Bridge watches the drop directory and republishes what lands there.
type Bridge struct {
	providers []Provider
	dir       string
	subs      *subscribers
	record    func(Notice) error
	display   func(DisplayFrame)
}

// NewBridge drains dir. record makes a notice durable before it is published
// and keeps the drop for retry when it fails; display takes MessageDisplay frames.
func NewBridge(dir string, record func(Notice) error, display func(DisplayFrame), providers ...Provider) (*Bridge, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Bridge{providers: providers, dir: dir, subs: newSubscribers(), record: record, display: display}, nil
}

func (b *Bridge) Dir() string { return b.dir }

// Subscribe returns a channel of notices and a function to stop listening.
func (b *Bridge) Subscribe() (<-chan Notice, func()) { return b.subs.add() }

// Follow calls fn with every notice until ctx ends, subscribing again when
// the bus drops a subscriber whose buffer filled.
func (b *Bridge) Follow(ctx context.Context, fn func(Notice)) {
	for ctx.Err() == nil {
		notices, stop := b.Subscribe()
	read:
		for {
			select {
			case <-ctx.Done():
				break read
			case n, ok := <-notices:
				if !ok {
					break read
				}
				fn(n)
			}
		}
		stop()
	}
}

// Run drains the drop directory until ctx is cancelled. Polling rather than
// fsnotify: drops are rare and 300ms is imperceptible on a prompt.
func (b *Bridge) Run(ctx context.Context) {
	// Drain once immediately so hooks that fired while the runtime was down are
	// not lost — which is the entire reason drops are files.
	b.drain()

	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.drain()
		}
	}
}

func (b *Bridge) drain() {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return
	}

	// Filenames are timestamp-prefixed, so sorting replays them in the order
	// the hooks actually fired.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(b.dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var d drop
		if json.Unmarshal(raw, &d) != nil {
			_ = os.Remove(path)
			continue
		}
		// Display frames take their own lane: no status, no recorder, no sweep.
		if d.Event == displayEvent {
			_ = os.Remove(path)
			if frame, ok := parseDisplay(d, b.providers); ok {
				b.display(frame)
			}
			continue
		}
		n, ok := parseDrop(d, b.providers)
		if !ok {
			_ = os.Remove(path)
			continue
		}
		if b.record(n) != nil {
			continue
		}
		_ = os.Remove(path)
		b.subs.publish(n)
	}
}

// drop is what the helper script writes.
type drop struct {
	Agent   string          `json:"agent"`
	Event   string          `json:"event"`
	Origin  string          `json:"origin"`
	AtMs    int64           `json:"at_ms"`
	Payload json.RawMessage `json:"payload"`
}

// origin is who submitted the prompt the hook fired for.
func (d drop) origin() Origin {
	if d.Origin == string(OriginApp) {
		return OriginApp
	}
	return OriginCLI
}

// time is when the hook fired, or now when the drop has no stamp.
func (d drop) time() time.Time {
	if d.AtMs > 0 {
		return time.UnixMilli(d.AtMs)
	}
	return time.Now()
}

func parseDrop(d drop, providers []Provider) (Notice, bool) {
	n := Notice{Event: d.Event, At: d.time(), Origin: d.origin()}

	// The payload's field names differ per agent, so every plausible spelling of
	// session id and cwd is accepted rather than branching on the agent — a new
	// hook event should degrade to a partly-filled notice, not a dropped one.
	var p struct {
		SessionID        string          `json:"session_id"`
		SessionID2       string          `json:"sessionId"`
		ThreadID         string          `json:"thread-id"`
		ThreadID2        string          `json:"thread_id"`
		Cwd              string          `json:"cwd"`
		Workspace        string          `json:"workspace"`
		Message          string          `json:"message"`
		Type             string          `json:"type"`
		HookEvent        string          `json:"hook_event_name"`
		PromptID         string          `json:"prompt_id"`
		TurnID           string          `json:"turn_id"`
		TurnID2          string          `json:"turn-id"`
		StopReason       *string         `json:"stop_reason"`
		Error            string          `json:"error"`
		NotificationType string          `json:"notification_type"`
		ToolName         string          `json:"tool_name"`
		ToolUseID        string          `json:"tool_use_id"`
		ToolInput        json.RawMessage `json:"tool_input"`
		Prompt           string          `json:"prompt"`
		Model            string          `json:"model"`
	}
	_ = json.Unmarshal(d.Payload, &p)

	n.SessionID = cmp.Or(p.SessionID, p.SessionID2, p.ThreadID, p.ThreadID2)
	n.Cwd = cmp.Or(p.Cwd, p.Workspace)
	n.PromptID = p.PromptID
	n.TurnID = cmp.Or(p.TurnID, p.TurnID2)
	n.StopReason, n.Error = p.StopReason, p.Error
	n.Message = p.Message
	n.Model = p.Model
	n.Prompt, n.ToolName, n.ToolUseID, n.ToolInput = p.Prompt, p.ToolName, p.ToolUseID, p.ToolInput
	if p.ToolName == questionTool {
		n.Questions = Questions(p.ToolInput)
	}
	if n.Event == "" {
		n.Event = cmp.Or(p.HookEvent, p.Type)
	}

	provider := find(providers, agent.Kind(strings.ToLower(d.Agent)))
	if provider == nil {
		return Notice{}, false
	}
	n.Agent = provider.Kind()
	n.Status = provider.HookStatus(n.Event, p.NotificationType, p.ToolName)
	if n.Status == agent.ChatUnknown {
		return Notice{}, false
	}
	if n.SessionID == "" {
		// Without a session id there is nothing to attach the state to.
		return Notice{}, false
	}
	return n, true
}
