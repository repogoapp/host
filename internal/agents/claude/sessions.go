package claude

import (
	"cmp"
	"os"
	"path/filepath"
	"strings"

	// bytedance/sonic rather than encoding/json: transcript parsing is the
	// host's biggest CPU cost and this decoder measured several times faster
	// on it. encoding/json stays for RawMessage (claudeblock.go).
	"github.com/bytedance/sonic"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

// Sessions reads ~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl. The
// directory name replaces "/" with "-" and is lossy, so cwd comes from the
// transcript's own `cwd` field and the name is only a fallback.
type Sessions struct {
	home  string
	cache *session.PeekCache[peekResult]

	// Sidechain reads a subagent's own file (subagents/agent-<id>.jsonl), where
	// every line is a sidechain; in a main file those lines are skipped. Parsing
	// needs no home, so a zero Sessions with only this set parses.
	Sidechain bool
}

// NewSessions reads the transcripts under home, Claude's config directory.
func NewSessions(home string) *Sessions {
	return &Sessions{home: home, cache: session.NewPeekCache[peekResult]()}
}

// Kind names the agent these sessions belong to.
func (c *Sessions) Kind() agent.Kind { return agent.KindClaude }

// List is one session per transcript file, titled and measured from a peek.
func (c *Sessions) List() ([]session.Meta, error) {
	projects := filepath.Join(c.home, "projects")

	dirs, err := os.ReadDir(projects)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // Claude simply is not installed.
		}
		return nil, err
	}

	var files []session.PeekFile
	var projectDirs []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(projects, d.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			files = append(files, session.PeekFile{Path: filepath.Join(projects, d.Name(), e.Name()), Info: info})
			projectDirs = append(projectDirs, d.Name())
		}
	}

	out := make([]session.Meta, len(files))
	for i, peeked := range session.PeekEach(files, c.peek) {
		f := files[i]
		cwd := peeked.cwd
		if cwd == "" {
			cwd = decodeProjectDir(projectDirs[i])
		}
		out[i] = session.Meta{
			ID:          strings.TrimSuffix(f.Info.Name(), ".jsonl"),
			Agent:       agent.KindClaude,
			Cwd:         cwd,
			Title:       peeked.title,
			Path:        f.Path,
			UpdatedAt:   f.Info.ModTime(),
			SizeBytes:   f.Info.Size(),
			ContextUsed: peeked.contextUsed,
			ContextSize: peeked.contextSize,
			Model:       peeked.model,
			Settings:    peeked.settings,
		}
	}
	return out, nil
}

// peek pulls cwd from a bounded head window and the newest title and context
// reading from a bounded tail window, which keeps listing thousands of
// sessions fast regardless of their size.
func (c *Sessions) peek(path string, info os.FileInfo) peekResult {
	if res, ok := c.cache.Get(path, info); ok {
		return res
	}

	var out peekResult
	session.ScanHead(path, func(line []byte) bool {
		var probe struct {
			Cwd string `json:"cwd"`
		}
		if sonic.Unmarshal(line, &probe) == nil && probe.Cwd != "" {
			out.cwd = probe.Cwd
			return false
		}
		return true
	})

	var customTitle string
	session.ScanTail(path, info.Size(), func(line []byte) bool {
		var probe struct {
			Type           string `json:"type"`
			AITitle        string `json:"aiTitle"`
			CustomTitle    string `json:"customTitle"`
			IsSidechain    bool   `json:"isSidechain"`
			PermissionMode string `json:"permissionMode"`
			Effort         string `json:"effort"`
			Message        struct {
				Model string       `json:"model"`
				Usage *claudeUsage `json:"usage"`
			} `json:"message"`
		}
		if sonic.Unmarshal(line, &probe) != nil {
			return true
		}
		// The tail is read forward, so the newest of each wins. A custom title
		// is the user's own (/rename, or an SDK client naming the session) and
		// beats the generated one whichever was written later.
		switch {
		case probe.Type == "custom-title" && probe.CustomTitle != "":
			customTitle = probe.CustomTitle
		case probe.Type == "ai-title" && probe.AITitle != "":
			out.title = probe.AITitle
		}
		// Sidechain rows are a subagent's own conversation. Its context is not
		// this chat's, and letting it win would report a Task tool's window to
		// someone looking at the chat that spawned it.
		if probe.Type == "assistant" && !probe.IsSidechain && probe.Message.Usage != nil {
			if used := probe.Message.Usage.contextTokens(); used > 0 {
				out.contextUsed = used
				// Read from the same row as the tokens: a chat whose model was switched must
				// measure against the window that request actually had.
				out.contextSize = contextWindowFor(probe.Message.Model)
			}
		}
		// A "<synthetic>" row is the CLI's own reply, not a model's.
		if probe.Type == "assistant" && !probe.IsSidechain && probe.Message.Model != "" && probe.Message.Model != "<synthetic>" {
			out.model = probe.Message.Model
		}
		if !probe.IsSidechain && probe.Message.Model != "<synthetic>" {
			applyClaudeSettings(&out.settings, probe.Type, probe.PermissionMode, probe.Effort, probe.Message.Usage)
		}
		return true
	})
	if customTitle != "" {
		out.title = customTitle
	}

	c.cache.Put(path, info, out)
	return out
}

// claudeUsage is one assistant request's token accounting.
type claudeUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	// "fast" when the request ran in fast mode, "standard" otherwise.
	Speed string `json:"speed"`
}

// applyClaudeSettings folds one line's settings into s, newest winning: the
// permission from user and permission-mode lines, effort and speed from
// replies. Plan sets the mode and leaves the permission it replaced standing.
func applyClaudeSettings(s *session.Settings, lineType, permission, effort string, usage *claudeUsage) {
	switch lineType {
	case "user", "permission-mode":
		switch permission {
		case "":
			return
		case "plan":
			s.Mode = "plan"
			return
		case "default":
			s.PermissionMode = agent.PermissionApprovalRequired
		case "acceptEdits", "auto":
			s.PermissionMode = agent.PermissionAutoAcceptEdits
		case "bypassPermissions":
			s.PermissionMode = agent.PermissionFullAccess
		}
		s.Mode = "agent"
	case "assistant":
		if effort != "" {
			s.ReasoningLevel = effort
		}
		if usage != nil && usage.Speed != "" {
			fast := usage.Speed == "fast"
			s.FastMode = &fast
		}
	}
}

// contextTokens sums the three input counts: cached tokens still occupied the
// window, and output is not context until the next request quotes it back
// inside its own input.
func (u claudeUsage) contextTokens() int64 {
	return u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens
}

// ParseLine is one transcript line's events and the line's timestamp.
func (c *Sessions) ParseLine(line []byte) ([]agent.Event, string) {
	var l claudeTranscriptLine
	if sonic.Unmarshal(line, &l) != nil {
		return nil, ""
	}
	if l.IsSidechain && !c.Sidechain {
		return nil, ""
	}
	var out []agent.Event
	switch l.Type {
	case "user":
		out = parseUser(l)
	case "attachment":
		out = parseQueued(l)
	case "assistant":
		out = parseAssistant(l)
	}
	// Every other type (system, ai-title, mode, file-history-snapshot…) is CLI
	// bookkeeping, not conversation.
	return out, l.Timestamp
}

// parseUser is a user record: the person's words, tool results sent back, or
// a background agent's report.
func parseUser(l claudeTranscriptLine) []agent.Event {
	// isMeta marks what the CLI wrote in the user's place: loaded skills,
	// "Continue from where you left off", hook feedback, image notes. The
	// compact summary is the conversation so far, restated for the model.
	if prompt, ok := peerPrompt(l); ok {
		return []agent.Event{{Kind: agent.EventUserMessage, Text: prompt, TurnID: l.PromptID}}
	}
	if l.IsMeta || l.IsCompactSummary {
		return nil
	}
	// Plain strings and arrays both go through userProse: the CLI writes its own
	// bookkeeping as plain-string user records too.
	if txt := strings.TrimSpace(l.Message.Content.Text); txt != "" {
		if result, ok := taskNotificationResult(txt); ok {
			return []agent.Event{{Kind: agent.EventToolResult, Tool: result}}
		}
		if prose := userProse(txt); prose != "" {
			return []agent.Event{{Kind: agent.EventUserMessage, Text: prose, TurnID: l.PromptID}}
		}
		return nil
	}
	// One record is one message however many text blocks it holds: a
	// prompt sent with attachments is the words plus a link per file,
	// and they read as one bubble, not one each.
	var out []agent.Event
	var prose []string
	var images []claudeBlock
	for _, b := range l.Message.Content.Blocks {
		switch b.Type {
		case "tool_result":
			out = append(out, agent.Event{Kind: agent.EventToolResult, TurnID: l.PromptID, Tool: &agent.ToolCall{
				CallID:  b.ToolUseID,
				Output:  b.contentText(),
				IsError: b.IsError,
			}})
		case "text":
			if p := userProse(b.Text); p != "" {
				prose = append(prose, p)
			}
		case "image":
			images = append(images, b)
		}
	}
	text := session.WithLinks(strings.Join(prose, "\n"), claudeImageLinks(images, prose))
	if text != "" {
		out = append(out, agent.Event{Kind: agent.EventUserMessage, Text: text, TurnID: l.PromptID})
	}
	return out
}

// parseQueued is a prompt typed while a turn ran, or a background task's
// notice, delivered into the running turn instead of as a user record.
func parseQueued(l claudeTranscriptLine) []agent.Event {
	a := l.Attachment
	if a.Type != "queued_command" || a.IsMeta {
		return nil
	}
	txt := strings.TrimSpace(a.Prompt.Text)
	if txt == "" {
		var parts []string
		for _, b := range a.Prompt.Blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		txt = strings.TrimSpace(strings.Join(parts, "\n"))
	}
	if result, ok := taskNotificationResult(txt); ok {
		return []agent.Event{{Kind: agent.EventToolResult, Tool: result}}
	}
	if a.CommandMode != "" && a.CommandMode != "prompt" {
		return nil
	}
	if prose := userProse(txt); prose != "" {
		return []agent.Event{{Kind: agent.EventUserMessage, Text: prose}}
	}
	return nil
}

// parseAssistant is one model reply's text, thinking and tool calls.
func parseAssistant(l claudeTranscriptLine) []agent.Event {
	// The CLI's own reply, not the model's: an API error ("529 Overloaded",
	// a session limit, an expired login) ends the turn as a failure; the
	// rest ("No response requested.") is not a reply at all.
	if l.Message.Model == "<synthetic>" {
		if !l.IsAPIError {
			return nil
		}
		var parts []string
		for _, b := range l.Message.Content.Blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return []agent.Event{{Kind: agent.EventTurnFailed,
			Error: cmp.Or(strings.Join(parts, "\n"), "The API returned an error.")}}
	}
	var out []agent.Event
	for _, b := range l.Message.Content.Blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, agent.Event{Kind: agent.EventText, Text: b.Text})
			}
		case "thinking":
			if b.Thinking != "" {
				out = append(out, agent.Event{Kind: agent.EventReasoning, Text: b.Thinking})
			}
		case "tool_use":
			out = append(out, agent.Event{Kind: agent.EventToolCall, Tool: &agent.ToolCall{
				CallID: b.ID,
				Name:   b.Name,
				Input:  b.Input,
			}})
		}
	}
	return out
}

// Records the CLI writes as if the user had spoken (task notifications, slash
// commands, interrupts, skill bodies, image notes); a prefix, so a message
// that merely mentions one is still a real message.
var syntheticUserPrefixes = []string{
	"<task-notification>",
	"<local-command-caveat>",
	"<local-command-stdout>",
	"<command-name>",
	"[Request interrupted by user",
	"Base directory for this skill: ",
	"[Image: source: ",
}

// A reminder block is context the CLI appends to a prompt: environment, memory,
// warnings. It is addressed to the agent, not written by the user, and reading
// it back in a transcript is how a chat ends up looking like a config file.
const (
	reminderOpen  = "<system-reminder>"
	reminderClose = "</system-reminder>"
)

// stripReminders removes every reminder block, and everything after one left
// unterminated by a truncated write. An index scan, not a regexp: a non-greedy
// match over long prompts was the import's second largest cost.
func stripReminders(s string) string {
	i := strings.Index(s, reminderOpen)
	if i < 0 {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i >= 0 {
		sb.WriteString(s[:i])
		s = s[i+len(reminderOpen):]
		j := strings.Index(s, reminderClose)
		if j < 0 {
			return sb.String()
		}
		s = s[j+len(reminderClose):]
		i = strings.Index(s, reminderOpen)
	}
	sb.WriteString(s)
	return sb.String()
}

// userProse reduces one user record to what the person typed, or empty. Done
// on the host, the only component reading transcripts, so every client gets
// the same filter.
func userProse(s string) string {
	s = strings.TrimSpace(stripReminders(s))
	if s == "" {
		return ""
	}
	for _, prefix := range syntheticUserPrefixes {
		if strings.HasPrefix(s, prefix) {
			return ""
		}
	}
	return s
}

// decodeProjectDir inverts Claude's lossy path encoding (every non-alphanumeric
// is "-") for a transcript with no cwd: the directory that exists on disk
// decides, else the plain dash-to-slash reading.
func decodeProjectDir(name string) string {
	if dir, ok := findEncodedDir("/", strings.TrimPrefix(name, "-")); ok {
		return dir
	}
	return strings.ReplaceAll(name, "-", "/")
}

// findEncodedDir finds the directory under parent whose path, encoded,
// reads rest.
func findEncodedDir(parent, rest string) (string, bool) {
	if rest == "" {
		return parent, true
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		enc := encodeProjectPart(e.Name())
		if !strings.HasPrefix(rest, enc) {
			continue
		}
		next := rest[len(enc):]
		if next != "" {
			if next[0] != '-' {
				continue
			}
			next = next[1:]
		}
		if dir, ok := findEncodedDir(filepath.Join(parent, e.Name()), next); ok {
			return dir, true
		}
	}
	return "", false
}

// encodeProjectPart is one path element as Claude spells it in a project
// directory's name.
func encodeProjectPart(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// peerPrompt is the prompt inside an inbox message: Claude writes it between
// its own header line and a note to the model, which the phone leaves out.
func peerPrompt(l claudeTranscriptLine) (string, bool) {
	text := l.Message.Content.Text
	if l.Origin.Kind != "peer" || !strings.HasPrefix(text, "Another Claude session sent a message") {
		return "", false
	}
	_, body, _ := strings.Cut(text, "\n")
	body, _, _ = strings.Cut(body, "\n\nThis came from another Claude session")
	body = strings.TrimSpace(body)
	return body, body != ""
}

// claudeTranscriptLine is the part of a transcript line the parser reads.
type claudeTranscriptLine struct {
	Type             string `json:"type"`
	Timestamp        string `json:"timestamp"`
	IsSidechain      bool   `json:"isSidechain"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	IsAPIError       bool   `json:"isApiErrorMessage"`
	// Origin names who sent a user record; "peer" is another process's inbox
	// message, which is how the host prompts a session open in a terminal.
	Origin struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	// PromptID names the turn: the CLI stamps it on a prompt and on every tool
	// result sent back while answering it. Replies carry none (claudeTurns).
	PromptID string `json:"promptId"`

	Message struct {
		Model   string        `json:"model"`
		Content claudeContent `json:"content"`
	} `json:"message"`

	Attachment struct {
		Type        string        `json:"type"`
		CommandMode string        `json:"commandMode"`
		IsMeta      bool          `json:"isMeta"`
		Prompt      claudeContent `json:"prompt"`
	} `json:"attachment"`
}

// claudeImageLinks links a user record's image blocks. A prompt sent from the
// app already carries a file link per attachment beside the inline copy, so a
// record with any link keeps only the links it has.
func claudeImageLinks(images []claudeBlock, prose []string) []string {
	for _, p := range prose {
		if strings.Contains(p, "](file://") {
			return nil
		}
	}
	var links []string
	for _, b := range images {
		if link := session.ImageLink(len(links)+1, b.Source.URL, b.Source.MediaType, b.Source.Data); link != "" {
			links = append(links, link)
		}
	}
	return links
}

// taskNotificationResult is a background agent's final report, which
// foldLateResults moves onto the call that answered "launched"; a notice with
// no report (a background shell command) is not a result.
func taskNotificationResult(s string) (*agent.ToolCall, bool) {
	if !strings.HasPrefix(s, "<task-notification>") {
		return nil, false
	}
	callID := tagText(s, "tool-use-id")
	result := tagText(s, "result")
	if callID == "" || result == "" {
		return nil, false
	}
	status := tagText(s, "status")
	return &agent.ToolCall{
		CallID:  callID,
		Output:  result,
		IsError: status != "" && status != "completed",
	}, true
}

// tagText is the text of the first <tag>…</tag> in s, trimmed.
func tagText(s, tag string) string {
	start, end := "<"+tag+">", "</"+tag+">"
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	// A report can quote tags of its own; its end is the last one.
	j := strings.Index(s, end)
	if tag == "result" {
		j = strings.LastIndex(s, end)
	}
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(s[:j])
}

// foldLateResults moves a result that arrives after its call already has one
// onto that first result, and drops the late event: a background agent answers
// "launched" at once and reports turns later, and the chat shows one row per call.
func foldLateResults(events []agent.Event) []agent.Event {
	first := map[string]int{}
	out := events[:0]
	for _, e := range events {
		if e.Kind == agent.EventToolResult && e.Tool != nil && e.Tool.CallID != "" {
			if i, ok := first[e.Tool.CallID]; ok {
				earlier := *out[i].Tool
				earlier.Output, earlier.IsError = e.Tool.Output, e.Tool.IsError
				out[i].Tool = &earlier
				continue
			}
			first[e.Tool.CallID] = len(out)
		}
		out = append(out, e)
	}
	return out
}

// peekResult is what List needs from one transcript, cached per file.
type peekResult struct {
	cwd   string
	title string

	// Context window as of the newest request in this file; see session.Meta.
	contextUsed int64
	contextSize int64

	// The model of the newest request; see session.Meta.Model.
	model string

	settings session.Settings
}

// Rename appends the line Claude's own /rename writes; the newest custom
// title wins when the transcript is read, here and in `claude --resume`.
func (c *Sessions) Rename(m session.Meta, title string) error {
	return session.AppendLine(m.Path, struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionID   string `json:"sessionId"`
	}{"custom-title", title, m.ID})
}
