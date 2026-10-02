package codex

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bytedance/sonic"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

// Sessions reads ~/.codex/sessions/YYYY/MM/DD/rollout-<iso>-<uuid>.jsonl.
// Prose comes from `response_item` only: `event_msg` mirrors it and would
// double every message.
type Sessions struct {
	home  string
	cache *session.PeekCache[peekResult]
}

// NewSessions reads the rollouts under home, Codex's config directory. Parsing
// needs no home, so a zero Sessions parses.
func NewSessions(home string) *Sessions {
	return &Sessions{home: home, cache: session.NewPeekCache[peekResult]()}
}

// Kind names the agent these sessions belong to.
func (c *Sessions) Kind() agent.Kind { return agent.KindCodex }

// List is one session per thread: a resumed thread's rollouts are its
// segments, and a subagent's rollout is not a session of its own.
func (c *Sessions) List() ([]session.Meta, error) {
	sessions := filepath.Join(c.home, "sessions")
	if _, err := os.Stat(sessions); os.IsNotExist(err) {
		return nil, nil // Codex simply is not installed.
	}

	titles := c.titles(filepath.Join(c.home, "session_index.jsonl"))

	var out []session.Meta
	byID := map[string]*session.Meta{}
	var order []string

	// The YYYY/MM/DD nesting is an implementation detail of how Codex files
	// rollouts; walking is more robust than reconstructing the date layout.
	var files []session.PeekFile
	err := filepath.WalkDir(sessions, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, session.PeekFile{Path: path, Info: info})
		return nil
	})

	for i, peeked := range session.PeekEach(files, c.peek) {
		path, info := files[i].Path, files[i].Info
		if peeked.id == "" || peeked.subagent != nil {
			continue
		}
		m := byID[peeked.id]
		if m == nil {
			m = &session.Meta{ID: peeked.id, Agent: agent.KindCodex, Cwd: peeked.cwd, Title: titles[peeked.id]}
			byID[peeked.id] = m
			order = append(order, peeked.id)
		}
		m.Paths = append(m.Paths, path)
		m.SizeBytes += info.Size()
		if info.ModTime().After(m.UpdatedAt) {
			m.UpdatedAt = info.ModTime()
			m.Path = path
			// Context comes from the newest segment only: a resumed rollout restates the
			// whole window rather than adding to it.
			if peeked.contextUsed > 0 {
				m.ContextUsed, m.ContextSize = peeked.contextUsed, peeked.contextSize
			}
			if peeked.model != "" {
				m.Model = peeked.model
			}
			m.Settings = peeked.settings.Or(m.Settings)
		}
		// An older segment's model and settings stand in until a newer one names its own.
		if m.Model == "" {
			m.Model = peeked.model
		}
		m.Settings = m.Settings.Or(peeked.settings)
	}

	for _, id := range order {
		m := byID[id]
		// Filenames embed an ISO timestamp, so lexical order is chronological —
		// which is the order the segments must be read in.
		sort.Strings(m.Paths)
		out = append(out, *m)
	}
	return out, err
}

// titles reads Codex's own index, which maps session id to the name shown in
// its picker. Absent or unreadable is fine — sessions just list untitled.
func (c *Sessions) titles(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), session.LineLimit)
	for sc.Scan() {
		var row struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if sonic.Unmarshal(sc.Bytes(), &row) == nil && row.ID != "" && row.Name != "" {
			out[row.ID] = row.Name
		}
	}
	return out
}

// Rename appends to Codex's index the row its own picker writes; the last
// row for an id is its name.
func (c *Sessions) Rename(m session.Meta, title string) error {
	return session.AppendLine(filepath.Join(c.home, "session_index.jsonl"), struct {
		ID         string `json:"id"`
		ThreadName string `json:"thread_name"`
		UpdatedAt  string `json:"updated_at"`
	}{m.ID, title, time.Now().UTC().Format(time.RFC3339Nano)})
}

// Context Codex sends in the user role that no user typed: the skills preamble,
// harness, plugin catalogue, sandbox policy and environment block. Prefix
// matched, and only the exact harness openings, because "You are " is prose.
var codexSyntheticUserPrefixes = []string{
	"<codex_internal_context>",
	"<codex_internal_context ",
	"<skills_instructions>",
	"<multi_agent_mode>",
	"<multi_agent_role>",
	"<recommended_plugins>",
	"<permissions instructions>",
	"<environment_context>",
	"<user_instructions>",
	"# AGENTS.md instructions for ",
	"The following is the Codex agent history",
	"You are `/root`, the primary agent",
	// The approval verdict a caller sends back, not prose.
	`{"risk_level":`,
	// Written after a Stop, telling the model the interruption was deliberate.
	"<turn_aborted>",
	// Multi-agent and voice harness notes addressed to the model.
	"<subagent_notification>",
	"<realtime_delegation>",
	"<no retained transcript delta entries>",
}

// isSyntheticCodexPrompt reports a user-role message that Codex wrote.
func isSyntheticCodexPrompt(text string) bool {
	for _, prefix := range codexSyntheticUserPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// stripCodexDirectives drops remark leaf directives such as
// `::inbox-item{title="…"}`: markup the Codex app renders as its own UI.
func stripCodexDirectives(text string) string {
	if !strings.Contains(text, "::") {
		return strings.TrimSpace(text)
	}
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if !isCodexDirective(strings.TrimSpace(line)) {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// isCodexDirective reports a line that is one whole `::name{…}` directive.
func isCodexDirective(line string) bool {
	name, _, ok := strings.Cut(strings.TrimPrefix(line, "::"), "{")
	if !ok || name == "" || !strings.HasPrefix(line, "::") || !strings.HasSuffix(line, "}") {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && r != '-' {
			return false
		}
	}
	return true
}

// peek reads until session_meta, Codex's first line, and the first turn's
// model, then the tail for the newest context reading and model. Cached per
// (size, mtime), so the tail pass costs once per file per change.
func (c *Sessions) peek(path string, info os.FileInfo) peekResult {
	if res, ok := c.cache.Get(path, info); ok {
		return res
	}

	var out peekResult
	session.ScanHead(path, func(line []byte) bool {
		var l struct {
			Type    string           `json:"type"`
			Payload codexSessionMeta `json:"payload"`
		}
		if sonic.Unmarshal(line, &l) != nil {
			return true
		}
		switch l.Type {
		case "session_meta":
			out.id, out.cwd = l.Payload.SessionID, l.Payload.Cwd
			out.subagent = l.Payload.subagent(path)
			// Read on to the first turn's model, which a long last turn can push
			// out of the tail window.
			return out.subagent == nil
		case "turn_context":
			out.model = codexTurnModel(line)
			return out.id == "" || out.model == ""
		}
		return true
	})

	session.ScanTail(path, info.Size(), func(line []byte) bool {
		var l struct {
			Type    string `json:"type"`
			Payload struct {
				Type  string `json:"type"`
				Model string `json:"model"`
				codexTurnSettings
				// Nullable: Codex writes token_count records with no info at
				// all, and a value type would silently read those as zero.
				Info *struct {
					Last struct {
						InputTokens int64 `json:"input_tokens"`
					} `json:"last_token_usage"`
					ContextWindow int64 `json:"model_context_window"`
				} `json:"info"`
			} `json:"payload"`
		}
		if sonic.Unmarshal(line, &l) != nil {
			return true
		}
		// Each turn restates its model, so the newest in the tail wins.
		if l.Type == "turn_context" && l.Payload.Model != "" {
			out.model = l.Payload.Model
		}
		if l.Type == "turn_context" || l.Type == "event_msg" && l.Payload.Type == "thread_settings_applied" {
			l.Payload.codexTurnSettings.apply(&out.settings)
		}
		if l.Type == "event_msg" && l.Payload.Type == "token_count" && l.Payload.Info != nil {
			// Last usage, not `total_token_usage`, which describes the bill rather than the
			// window. input_tokens alone, because Codex's already contains
			// cached_input_tokens.
			if used := l.Payload.Info.Last.InputTokens; used > 0 {
				out.contextUsed = used
				out.contextSize = l.Payload.Info.ContextWindow
			}
		}
		return true
	})

	c.cache.Put(path, info, out)
	return out
}

// codexTurnSettings is what a turn_context line records of the turn's
// configuration, and the service tier a thread_settings_applied line carries.
type codexTurnSettings struct {
	Effort        string `json:"effort"`
	SandboxPolicy struct {
		Type string `json:"type"`
	} `json:"sandbox_policy"`
	PermissionProfile struct {
		Type string `json:"type"`
	} `json:"permission_profile"`
	CollaborationMode struct {
		Mode string `json:"mode"`
	} `json:"collaboration_mode"`
	ThreadSettings struct {
		ServiceTier string `json:"service_tier"`
	} `json:"thread_settings"`
}

// apply folds the line into s, newest winning. The sandbox decides the
// permission, as it does for the app's modes going out (agent.modeIDs):
// read-only asks, workspace-write edits, and no sandbox is full access.
func (t codexTurnSettings) apply(s *session.Settings) {
	if t.Effort != "" {
		s.ReasoningLevel = t.Effort
	}
	switch t.CollaborationMode.Mode {
	case "plan":
		s.Mode = "plan"
	case "default":
		s.Mode = "agent"
	}
	switch {
	case t.SandboxPolicy.Type == "danger-full-access" || t.PermissionProfile.Type == "disabled":
		s.PermissionMode = agent.PermissionFullAccess
	case t.SandboxPolicy.Type == "workspace-write":
		s.PermissionMode = agent.PermissionAutoAcceptEdits
	case t.SandboxPolicy.Type == "read-only":
		s.PermissionMode = agent.PermissionApprovalRequired
	}
	if tier := t.ThreadSettings.ServiceTier; tier != "" {
		fast := fastTier(tier)
		s.FastMode = &fast
	}
}

// codexTurnModel is the model a turn_context line names.
func codexTurnModel(line []byte) string {
	var l struct {
		Payload struct {
			Model string `json:"model"`
		} `json:"payload"`
	}
	if sonic.Unmarshal(line, &l) != nil {
		return ""
	}
	return l.Payload.Model
}

// ParseLine is one rollout line's events and the line's timestamp.
func (c *Sessions) ParseLine(line []byte) ([]agent.Event, string) {
	var l codexLine
	if sonic.Unmarshal(line, &l) != nil {
		return nil, ""
	}
	switch l.Type {
	case "event_msg":
		return parseEventMsg(l), l.Timestamp
	case "response_item":
		return parseResponseItem(l), l.Timestamp
	}
	return nil, ""
}

// parseEventMsg is a turn's lifecycle: a transcript read back has no Manager,
// so the file is the only record that a turn started or how it ended.
func parseEventMsg(l codexLine) []agent.Event {
	switch l.Payload.Type {
	case "task_started":
		return []agent.Event{{Kind: agent.EventTurnStarted, TurnID: l.Payload.TurnID}}

	case "task_complete":
		return []agent.Event{{Kind: agent.EventTurnFinished, TurnID: l.Payload.TurnID}}

	// Three event types end a turn: `task_complete`, `turn_aborted` (Stop) and
	// `error`. Aborts are common enough that inferring them from silence is wrong.
	case "turn_aborted":
		return []agent.Event{{Kind: agent.EventTurnFailed, TurnID: l.Payload.TurnID, Error: "aborted"}}

	case "error":
		return []agent.Event{{Kind: agent.EventTurnFailed, TurnID: l.Payload.TurnID,
			Error: cmp.Or(l.Payload.Message, "codex reported an error")}}

	// A goal's objective is the only record of what the user typed: its turns
	// open with a synthetic prompt instead. Pausing or resuming moves updatedAt.
	case "thread_goal_updated":
		g := l.Payload.Goal
		if text := strings.TrimSpace(g.Objective); text != "" && g.UpdatedAt == g.CreatedAt {
			return []agent.Event{{Kind: agent.EventUserMessage, Text: text}}
		}
	}
	// token_count and the rest are bookkeeping.
	return nil
}

// parseResponseItem is the conversation itself: messages, tool calls and
// their results, web searches and reasoning summaries.
func parseResponseItem(l codexLine) []agent.Event {
	switch l.Payload.Type {
	case "message":
		return parseMessage(l)

	case "custom_tool_call", "function_call":
		return []agent.Event{{Kind: agent.EventToolCall, Tool: &agent.ToolCall{
			CallID: l.Payload.CallID,
			Name:   l.Payload.Name,
			Input:  rawOrString(l.Payload.Input, l.Payload.Arguments),
		}}}

	case "custom_tool_call_output", "function_call_output":
		return []agent.Event{{Kind: agent.EventToolResult, Tool: &agent.ToolCall{
			CallID: l.Payload.CallID,
			Output: flattenOutput(l.Payload.Output),
		}}}

	case "web_search_call":
		return codexWebSearch(l.Payload.ID, l.Payload.Status, l.Payload.Action, l.Timestamp)

	case "reasoning":
		// `summary` is the readable trace; `encrypted_content` is opaque to
		// everyone but the provider and is deliberately not surfaced.
		var sb strings.Builder
		for _, s := range l.Payload.Summary {
			sb.WriteString(s.Text)
		}
		if sb.Len() > 0 {
			return []agent.Event{{Kind: agent.EventReasoning, Text: sb.String()}}
		}
	}
	return nil
}

// parseMessage is a user prompt or an assistant reply. Assistant parts are
// chunks of one text and run together; a user's are items (the words, a link
// per file), and joined bare they would read as one word.
func parseMessage(l codexLine) []agent.Event {
	var parts []string
	for _, part := range l.Payload.Content {
		if part.Type == "input_text" || part.Type == "output_text" || part.Type == "text" {
			parts = append(parts, part.Text)
		}
	}
	switch l.Payload.Role {
	case "user":
		if value := joinLines(parts); value != "" && isSyntheticCodexPrompt(value) {
			return nil
		}
		if value := codexUserPrompt(l.Payload.Content); value != "" {
			return []agent.Event{{Kind: agent.EventUserMessage, Text: value, TurnID: l.Payload.TurnID}}
		}
	case "assistant":
		if value := stripCodexDirectives(strings.Join(parts, "")); value != "" {
			return []agent.Event{{Kind: agent.EventText, Text: value, TurnID: l.Payload.TurnID}}
		}
	}
	return nil
}

// rawOrString is a tool call's input: custom tools send JSON, function calls
// a string of arguments, which is kept as a JSON string.
func rawOrString(input json.RawMessage, arguments string) json.RawMessage {
	if len(input) > 0 {
		return input
	}
	if arguments == "" {
		return nil
	}
	// encoding/json on purpose: its HTML escaping is what every stored
	// content_hash was computed with, and sonic's Marshal does not escape.
	b, err := json.Marshal(arguments)
	if err != nil {
		return nil
	}
	return b
}

// flattenOutput copes with output being a bare string on some tools and an
// object with a nested field on others.
func flattenOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if sonic.Unmarshal(raw, &s) == nil {
		// Computer-use tools answer with a JSON-encoded string of parts, and
		// a screenshot part is megabytes of base64 no transcript row can carry.
		if strings.HasPrefix(s, "[") {
			if parts, ok := flattenParts([]byte(s)); ok {
				return parts
			}
		}
		return s
	}
	var obj struct {
		Output  string `json:"output"`
		Content string `json:"content"`
		Text    string `json:"text"`
	}
	if sonic.Unmarshal(raw, &obj) == nil {
		for _, v := range []string{obj.Output, obj.Content, obj.Text} {
			if v != "" {
				return v
			}
		}
	}
	if parts, ok := flattenParts(raw); ok {
		return parts
	}
	return string(raw)
}

// flattenParts joins an array of content parts, an image reading "[image]".
func flattenParts(raw []byte) (string, bool) {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if sonic.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return "", false
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type == "input_image" {
			out = append(out, "[image]")
		} else if part.Text != "" {
			out = append(out, part.Text)
		}
	}
	return strings.Join(out, "\n"), true
}

// codexLine is the part of a rollout line the parser reads.
type codexLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type string `json:"type"`

		// event_msg
		Message string `json:"message"`
		TurnID  string `json:"turn_id"`
		Goal    struct {
			Objective string `json:"objective"`
			CreatedAt int64  `json:"createdAt"`
			UpdatedAt int64  `json:"updatedAt"`
		} `json:"goal"`

		// response_item
		Role    string      `json:"role"`
		Content []codexPart `json:"content"`

		// web_search_call
		ID     string          `json:"id"`
		Status string          `json:"status"`
		Action json.RawMessage `json:"action"`

		// tool calls, their output, and reasoning
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
		Summary   []struct {
			Text string `json:"text"`
		} `json:"summary"`
	} `json:"payload"`
}

// joinLines puts each non-empty part on its own line.
func joinLines(parts []string) string {
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

// codexSessionMeta is a rollout's first line. A subagent's names its parent
// twice over: `parent_thread_id` with `thread_source: "subagent"` (0.14x), and
// `source.subagent.thread_spawn` (every version that spawns).
type codexSessionMeta struct {
	SessionID    string          `json:"session_id"`
	ID           string          `json:"id"`
	Cwd          string          `json:"cwd"`
	ParentThread string          `json:"parent_thread_id"`
	ThreadSource string          `json:"thread_source"`
	AgentPath    string          `json:"agent_path"`
	Nickname     string          `json:"agent_nickname"`
	Source       json.RawMessage `json:"source"`
	// Where the child's own work starts: a forked subagent begins with a copy
	// of its parent's history, which this many lines hold.
	HistoryStart int `json:"subagent_history_start_ordinal"`
}

// subagent is the rollout at path as a subagent's, or nil when it is a
// thread of its own.
func (m codexSessionMeta) subagent(path string) *codexSubagent {
	var src struct {
		Subagent struct {
			ThreadSpawn struct {
				ParentThread string `json:"parent_thread_id"`
				AgentPath    string `json:"agent_path"`
				Nickname     string `json:"agent_nickname"`
				Role         string `json:"agent_role"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	if len(m.Source) > 0 && m.Source[0] == '{' {
		_ = sonic.Unmarshal(m.Source, &src)
	}
	spawn := src.Subagent.ThreadSpawn
	parent := cmp.Or(m.ParentThread, spawn.ParentThread)
	if parent == "" && m.ThreadSource != "subagent" {
		return nil
	}
	return &codexSubagent{
		ThreadID:     cmp.Or(m.ID, m.SessionID),
		ParentThread: parent,
		AgentPath:    cmp.Or(m.AgentPath, spawn.AgentPath),
		Nickname:     cmp.Or(m.Nickname, spawn.Nickname),
		Role:         spawn.Role,
		HistoryStart: m.HistoryStart,
		Path:         path,
	}
}

// codexSubagent is one spawned agent's rollout.
type codexSubagent struct {
	ThreadID     string
	ParentThread string
	AgentPath    string
	Nickname     string
	Role         string
	HistoryStart int
	Path         string
}

// codexPart is one content part of a message.
type codexPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
}

// codexUserPrompt is what the user sent: their words, unwrapped from Codex's
// IDE or mobile headings, with a link per attached file or image; a prompt that
// already links its files keeps only those, the inline images being copies.
func codexUserPrompt(content []codexPart) string {
	var texts, links, imageURLs []string
	linked := false
	for _, part := range content {
		switch part.Type {
		case "input_text", "text":
			if isImageTag(part.Text) {
				continue
			}
			text, files := unwrapCodexRequest(part.Text)
			texts = append(texts, text)
			links = append(links, files...)
			linked = linked || len(files) > 0 || strings.Contains(text, "](file://")
		case "input_image":
			imageURLs = append(imageURLs, part.ImageURL)
		}
	}
	if !linked {
		for _, url := range imageURLs {
			if link := session.ImageLink(len(links)+1, url, "", ""); link != "" {
				links = append(links, link)
			}
		}
	}
	return strings.TrimSpace(session.WithLinks(joinLines(texts), links))
}

// isImageTag is one of the parts Codex writes around a pasted image:
// `<image name=[Image #1]>`, `<image>`, `</image>`.
func isImageTag(s string) bool {
	s = strings.TrimSpace(s)
	return s == "<image>" || s == "</image>" || (strings.HasPrefix(s, "<image name=") && strings.HasSuffix(s, ">"))
}

// codexRequestHeadings open the request in a prompt from Codex's IDE extension
// or mobile app; newer mobile builds drop "for Codex".
var codexRequestHeadings = []string{"## My request for Codex:", "## My request:"}

// unwrapCodexRequest splits a prompt from Codex's IDE extension or mobile app
// (context headings, then "## My request…:") into the request and a link per
// file it mentions; any other prompt comes back whole.
func unwrapCodexRequest(s string) (string, []string) {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "# Context from my IDE setup:") && !strings.HasPrefix(trimmed, "# Files mentioned by the user:") {
		return s, nil
	}
	var head, request string
	ok := false
	for _, heading := range codexRequestHeadings {
		if head, request, ok = strings.Cut(trimmed, heading); ok {
			break
		}
	}
	if !ok {
		return s, nil
	}
	var links []string
	inFiles := false
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "# "):
			inFiles = strings.HasPrefix(line, "# Files mentioned by the user:")
		case inFiles && strings.HasPrefix(line, "## "):
			name, path, ok := strings.Cut(strings.TrimPrefix(line, "## "), ": ")
			if path = strings.TrimSpace(path); ok && name != "" && strings.HasPrefix(path, "/") {
				links = append(links, "[@"+name+"](file://"+path+")")
			}
		}
	}
	return strings.TrimSpace(request), links
}

// codexWebSearch is one web_search_call as a web_search (query) or web_fetch
// (page) call and, once done, its result; before 0.14x Codex gives it no id,
// so one is made from its line.
func codexWebSearch(id, status string, action json.RawMessage, timestamp string) []agent.Event {
	var a struct {
		Type string `json:"type"`
	}
	_ = sonic.Unmarshal(action, &a)
	name := "web_search"
	if a.Type == "open_page" || a.Type == "find_in_page" {
		name = "web_fetch"
	}
	callID := cmp.Or(id, fmt.Sprintf("ws:%s:%016x", timestamp, session.FNV64(string(action))))
	input := action
	if len(input) == 0 || input[0] != '{' {
		input = nil
	}
	events := []agent.Event{{Kind: agent.EventToolCall, Tool: &agent.ToolCall{CallID: callID, Name: name, Input: input}}}
	if status == "completed" || status == "failed" {
		events = append(events, agent.Event{Kind: agent.EventToolResult, Tool: &agent.ToolCall{
			CallID: callID, Name: name, IsError: status == "failed",
		}})
	}
	return events
}

// peekResult is what List needs from one rollout, cached per file.
type peekResult struct {
	cwd string
	id  string

	// Context window as of the newest request in this file; see session.Meta.
	contextUsed int64
	contextSize int64

	// The model of the newest request; see session.Meta.Model.
	model string

	settings session.Settings

	// A rollout a subagent wrote: its own thread, spawned by parent. Not a
	// chat of its own, and not part of its parent's, which it shares a
	// session_id with; read only through Store.Subagent.
	subagent *codexSubagent
}
