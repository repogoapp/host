package antigravity

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agents/acp"
	"github.com/repogoapp/host/internal/session"
)

// history is Antigravity's chats: the host's files for RepoGo's, and agy's
// logs (brain/<id>/.system_generated/logs/transcript_full.jsonl) for the ones
// agy ran on its own. A chat RepoGo started names its agy conversation, which
// is left out; one RepoGo adopted (a terminal chat continued from the phone)
// has agy's id as its own and is read from agy's log, which has every turn.
type history struct {
	host *acp.Sessions
	p    *Provider
}

func (h *history) Kind() agent.Kind { return agent.KindAntigravity }

func (h *history) List() ([]session.Meta, error) {
	own, err := h.host.List()
	if err != nil {
		return nil, err
	}
	links := h.host.Links()
	out := make([]session.Meta, 0, len(own))
	for _, m := range own {
		if l, ok := links[m.ID]; ok && l.Chat == m.ID {
			continue // adopted: listed from agy's log below
		}
		out = append(out, m)
	}
	entries, err := os.ReadDir(filepath.Join(h.p.home, "brain"))
	if err != nil {
		return out, nil
	}
	conversations := h.p.conversations()
	for _, e := range entries {
		id := e.Name()
		if l, ok := links[id]; ok && l.Chat != id {
			continue // RepoGo's own chat, listed from the host's file
		}
		c := conversations[id]
		path := filepath.Join(h.p.home, "brain", id, ".system_generated", "logs", usageLog)
		info, err := os.Stat(path)
		if err != nil || c.cwd == "" {
			continue
		}
		// A chat whose folder is gone (a test's) has nowhere to continue.
		if dir, err := os.Stat(c.cwd); err != nil || !dir.IsDir() {
			continue
		}
		title := c.title
		if l, ok := links[id]; ok && l.Title != "" {
			title = l.Title
		}
		out = append(out, session.Meta{
			ID: id, Agent: agent.KindAntigravity, Cwd: c.cwd, Title: title,
			Path: path, UpdatedAt: info.ModTime(), SizeBytes: info.Size(),
		})
	}
	return out, nil
}

// Rename keeps the user's title in the host's file, adopting a chat that
// has none, so agy's own log is never written to.
func (h *history) Rename(m session.Meta, title string) error {
	if filepath.Base(m.Path) != usageLog {
		return h.host.Rename(m, title)
	}
	path, err := h.host.Adopt(m.ID, m.Cwd)
	if err != nil {
		return err
	}
	return h.host.Rename(session.Meta{ID: m.ID, Path: path}, title)
}

// ParseLine reads a line of either file: agy's steps carry a step_index.
func (h *history) ParseLine(line []byte) ([]agent.Event, string) {
	if bytes.Contains(line, []byte(`"step_index"`)) {
		return parseLogStep(line)
	}
	return h.host.ParseLine(line)
}

// Normalize gives agy's rows their turns and results their calls; the host's
// own rows have both already. A step logged twice (running, then done) keeps
// its last reading.
func (h *history) Normalize(events []agent.Event) []agent.Event {
	last := map[string]int{}
	for i, e := range events {
		if isLogResult(e) {
			last[e.Tool.CallID] = i
		}
	}
	kept := events[:0]
	for i, e := range events {
		if !isLogResult(e) || last[e.Tool.CallID] == i {
			kept = append(kept, e)
		}
	}
	return (&matcher{}).apply(kept)
}

// NewStreamNormalizer does the same for a watched chat as agy writes it: a
// step's second reading updates the call its first was matched to.
func (h *history) NewStreamNormalizer() func([]agent.Event) []agent.Event {
	return (&matcher{}).apply
}

// logTurn and logResult mark agy's rows until Normalize settles them: a
// prompt's turn is its step, and a result names its step until it is matched
// to its call.
const (
	logTurn   = "agy-"
	logResult = "agy-result-"
)

// parseLogStep is one step of agy's log as events: a prompt, a model call's
// thinking, text and tool calls, or a tool's result. Steps the system adds
// (instructions, compaction) show nothing.
func parseLogStep(line []byte) ([]agent.Event, string) {
	var s struct {
		Index     int64  `json:"step_index"`
		Type      string `json:"type"`
		Status    string `json:"status"`
		CreatedAt string `json:"created_at"`
		Content   string `json:"content"`
		Thinking  string `json:"thinking"`
		Error     string `json:"error"`
		ToolCalls []struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal(line, &s) != nil {
		return nil, ""
	}
	step := strconv.FormatInt(s.Index, 10)
	var out []agent.Event
	switch s.Type {
	case "USER_INPUT":
		if text := userRequest(s.Content); text != "" {
			out = append(out, agent.Event{Kind: agent.EventUserMessage, TurnID: logTurn + step, Text: text})
		}
	case "PLANNER_RESPONSE":
		if t := strings.TrimSpace(s.Thinking); t != "" {
			out = append(out, agent.Event{Kind: agent.EventReasoning, Text: t})
		}
		if t := strings.TrimSpace(s.Content); t != "" {
			out = append(out, agent.Event{Kind: agent.EventText, Text: t})
		}
		for i, call := range s.ToolCalls {
			out = append(out, agent.Event{Kind: agent.EventToolCall, Tool: &agent.ToolCall{
				CallID: logTurn + step + "-" + strconv.Itoa(i), Name: call.Name, Input: call.Args,
			}})
		}
	case "GENERIC":
		output := toolText(s.Content)
		if output == "" {
			output = s.Error
		}
		out = append(out, agent.Event{Kind: agent.EventToolResult, Tool: &agent.ToolCall{
			CallID: logResult + step, Output: output, IsError: s.Status == "ERROR",
		}})
	}
	return out, s.CreatedAt
}

// userRequest is what the user typed: agy wraps it in <USER_REQUEST> with
// metadata after it.
func userRequest(content string) string {
	if _, rest, ok := strings.Cut(content, "<USER_REQUEST>"); ok {
		content, _, _ = strings.Cut(rest, "</USER_REQUEST>")
	}
	return strings.TrimSpace(content)
}

// toolText is a tool's output without the timing lines agy puts first.
func toolText(content string) string {
	lines := strings.Split(content, "\n")
	for len(lines) > 0 && (strings.HasPrefix(lines[0], "Created At:") || strings.HasPrefix(lines[0], "Completed At:")) {
		lines = lines[1:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// matcher gives agy's rows the turn of the prompt before them, and each
// result the call it answers: the oldest call still waiting, as agy runs them
// in order. It keeps both across reads of a streaming log.
type matcher struct {
	turn    string
	waiting []*agent.ToolCall
	// byStep is the call each result step was matched to.
	byStep map[string]*agent.ToolCall
}

func (m *matcher) apply(events []agent.Event) []agent.Event {
	if m.byStep == nil {
		m.byStep = map[string]*agent.ToolCall{}
	}
	out := events[:0]
	for _, e := range events {
		if e.TurnID != "" {
			m.turn = e.TurnID
		} else {
			e.TurnID = m.turn
		}
		switch {
		case e.Kind == agent.EventToolCall && e.Tool != nil && strings.HasPrefix(e.Tool.CallID, logTurn):
			m.waiting = append(m.waiting, e.Tool)
		case isLogResult(e):
			call := m.byStep[e.Tool.CallID]
			if call == nil {
				if len(m.waiting) == 0 {
					continue // no call waiting for it
				}
				call, m.waiting = m.waiting[0], m.waiting[1:]
				m.byStep[e.Tool.CallID] = call
			}
			result := *e.Tool
			result.CallID, result.Name = call.CallID, call.Name
			e.Tool = &result
		}
		out = append(out, e)
	}
	return out
}

// isLogResult reports a result from agy's log not yet matched to its call.
func isLogResult(e agent.Event) bool {
	return e.Kind == agent.EventToolResult && e.Tool != nil && strings.HasPrefix(e.Tool.CallID, logResult)
}

// conversation is what agy's metadata says about one: its folder and title.
type conversation struct{ cwd, title string }

// conversations is agy's conversations by id, from the metadata it caches
// beside them; one it no longer lists has none.
func (p *Provider) conversations() map[string]conversation {
	raw, err := os.ReadFile(filepath.Join(p.home, "cache", "conversation_metadata.json"))
	if err != nil {
		return nil
	}
	var file struct {
		Conversations map[string]struct {
			Summary struct {
				Title         string   `json:"Title"`
				WorkspaceURIs []string `json:"WorkspaceURIs"`
			} `json:"summary"`
		} `json:"conversations"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return nil
	}
	out := make(map[string]conversation, len(file.Conversations))
	for id, c := range file.Conversations {
		conv := conversation{title: c.Summary.Title}
		for _, uri := range c.Summary.WorkspaceURIs {
			if dir, ok := strings.CutPrefix(uri, "file://"); ok && dir != "" {
				conv.cwd = dir
				break
			}
		}
		out[id] = conv
	}
	return out
}
