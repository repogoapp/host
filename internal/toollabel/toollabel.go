// Package toollabel names tool calls for the chat: an SF Symbol and a line for
// each state. It holds the vocabulary every agent shares (generic tool names,
// MCP tools) and the builders providers label with.
package toollabel

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Label is one tool call as the chat draws it.
type Label struct {
	Icon   string `json:"icon"`
	Labels Lines  `json:"labels"`
}

// Lines is what the row says while the call runs, after it completes, and
// when it fails.
type Lines struct {
	Active    string `json:"active"`
	Completed string `json:"completed"`
	Error     string `json:"error"`
}

// New is a label from its icon and three lines.
func New(icon, active, completed, failed string) Label {
	return Label{Icon: icon, Labels: Lines{Active: active, Completed: completed, Error: failed}}
}

// Input reads a tool call's input. A JSON object comes back as args; a string
// holding an object (Codex's function arguments) is read as one; any other
// string is the raw text a custom tool takes, a patch or a script.
func Input(raw json.RawMessage) (args map[string]any, text string) {
	if len(raw) == 0 {
		return nil, ""
	}
	if json.Unmarshal(raw, &args) == nil {
		return args, ""
	}
	if json.Unmarshal(raw, &text) != nil {
		return nil, ""
	}
	if strings.HasPrefix(text, "{") && json.Unmarshal([]byte(text), &args) == nil {
		return args, ""
	}
	return nil, text
}

// Shared labels the names no single provider owns: generic tool names, MCP
// tools, and anything unknown.
func Shared(name string, args map[string]any) Label {
	switch name {
	case "read":
		return Read(args)
	case "edit", "write":
		return Edit(args)
	case "delete":
		return Delete(args)
	case "move":
		return New("arrow.right.doc.on.clipboard", "Moving file", "Moved file", "Failed to move file")
	case "execute", "bash":
		return Shell(args)
	case "ls":
		return List(args)
	case "grep", "search":
		return Grep(args)
	case "glob":
		return Glob(args)
	case "think":
		return New("brain", "Thinking", "Thought", "Thinking failed")
	case "switch_mode":
		return New("arrow.triangle.2.circlepath", "Switching mode", "Switched mode", "Failed to switch mode")
	case "webSearch", "web_search", "google_search", "perplexity_search":
		return WebSearch(String(args, "query"))
	case "webFetch", "web_fetch":
		return WebFetch(args)
	case "fetch":
		return Fetch(args)
	case "task", "agent", "spawnAgent":
		return Subagent(args)
	case "askUserQuestion":
		return Question(args)
	case "browser":
		return Browser(args)
	case "toolSearch":
		return ToolSearch(args, func(name string) Label { return Shared(name, nil) })
	case "enterplanmode", "enterPlanMode":
		return EnterPlanMode()
	case "exitplanmode", "exitPlanMode":
		return ExitPlanMode()
	}
	if label, ok := mcp(name); ok {
		return label
	}
	return fallback(name)
}

// Read is a file read: the file's name, or "files" without one.
func Read(args map[string]any) Label {
	file := FileName(filePath(args), "files")
	return New("doc.text.magnifyingglass", "Reading "+file, "Read "+file, "Reading files attempted")
}

// Edit is a file edit or write: both read as editing the file.
func Edit(args map[string]any) Label {
	file := FileName(filePath(args), "file")
	return New("square.and.pencil", "Editing "+file, "Edited "+file, "Editing file attempted")
}

// Delete is a file deletion.
func Delete(args map[string]any) Label {
	file := FileName(filePath(args), "file")
	return New("trash", "Deleting "+file, "Deleted "+file, "Deleting file attempted")
}

// Shell is a command: the agent's own description of it when it gave one
// (Claude's Bash `description`), else the command itself.
func Shell(args map[string]any) Label {
	line := String(args, "description")
	if line == "" {
		line = shellCommand(args)
	}
	line = Preview(line, 40, "command")
	return New("terminal", line, line, "Command attempted")
}

// shellCommand is the command a shell call runs: `command` or `cmd` as text,
// or an argv, whose `sh -lc <script>` form reads as the script.
func shellCommand(args map[string]any) string {
	if command, ok := args["command"].(string); ok {
		return command
	}
	if cmd, ok := args["cmd"].(string); ok {
		return cmd
	}
	var argv []string
	for _, v := range list(args["command"]) {
		if s, ok := v.(string); ok {
			argv = append(argv, s)
		}
	}
	if len(argv) == 3 && argv[1] == "-lc" {
		return argv[2]
	}
	return strings.Join(argv, " ")
}

// List is a directory listing.
func List(args map[string]any) Label {
	dir := shortPath(String(args, "path"))
	return New("folder", "Listing "+dir, "Listed "+dir, "Listing directory attempted")
}

// Grep is a search of file contents.
func Grep(args map[string]any) Label {
	pattern := Preview(String(args, "pattern"), 40, "code")
	return New("magnifyingglass", "Searching "+pattern, "Searched "+pattern, "Grep search attempted")
}

// Glob is a search for file names.
func Glob(args map[string]any) Label {
	pattern := Preview(String(args, "pattern"), 40, "files")
	return New("doc.text.magnifyingglass", "Searching "+pattern, "Searched "+pattern, "Glob search attempted")
}

// WebSearch is a search of the web for query.
func WebSearch(query string) Label {
	q := Preview(query, 40, "web")
	return New("globe", "Searching "+q, "Searched "+q, "Web search attempted")
}

// WebFetch is a page fetched by URL.
func WebFetch(args map[string]any) Label {
	u := Preview(String(args, "url"), 50, "URL")
	return New("arrow.down.circle", "Fetching "+u, "Fetched "+u, "Fetch attempted")
}

// Fetch is an HTTP request.
func Fetch(args map[string]any) Label {
	method := String(args, "method")
	if method == "" {
		method = "GET"
	}
	u := Preview(String(args, "url"), 50, "URL")
	return New("network", method+" "+u, "Sent "+method+" "+u, "Request attempted")
}

// Subagent is a call that starts another agent, titled by what it was asked
// to do.
func Subagent(args map[string]any) Label {
	title := Preview(subagentTitle(args), 40, "a subagent")
	return New("person.2", "Running "+title, "Ran "+title, "Subagent failed: "+title)
}

// subagentTitle is what a subagent was asked to do, in a few words: Claude's
// `description`, Codex's task name, else the prompt's first line.
func subagentTitle(args map[string]any) string {
	if description := trimmed(args, "description"); description != "" {
		return description
	}
	if task := trimmed(args, "task_name"); task != "" {
		leaf := task[strings.LastIndex(task, "/")+1:]
		return capitalize(strings.NewReplacer("_", " ", "-", " ").Replace(leaf))
	}
	prompt := trimmed(args, "prompt")
	if prompt == "" {
		prompt = trimmed(args, "message")
	}
	line, _, _ := strings.Cut(prompt, "\n")
	if utf8.RuneCountInString(line) > 60 {
		return string([]rune(line)[:59]) + "…"
	}
	return line
}

// Todos is a todo list written whole.
func Todos(args map[string]any) Label {
	n := len(list(args["todos"]))
	count := fmt.Sprintf("%d todos", n)
	if n == 1 {
		count = "1 todo"
	}
	return New("checklist", "Updating "+count, "Updated "+count, "Updating todos attempted")
}

// Question is a question put to the user, titled by its header.
func Question(args map[string]any) Label {
	var first map[string]any
	if questions := dicts(args["questions"]); len(questions) > 0 {
		first = questions[0]
	}
	title, ok := first["header"].(string)
	if !ok {
		title = String(first, "question")
	}
	title = Preview(title, 48, "Question")
	return New("questionmark.bubble", title, "Answered question", "Question attempted")
}

// Browser is RepoGo's browser, driven by the agent.
func Browser(args map[string]any) Label {
	if String(args, "action") == "snapshot" {
		return New("safari", "Reading the browser", "Read the browser", "Browser action failed")
	}
	return New("safari", "Controlling the browser", "Controlled the browser", "Browser action failed")
}

// ToolSearch is tools switched on by name (`select:WebSearch,WebFetch`) or
// found by keyword. Named tools read as labelOf names them, and take their
// icon when they share one, so enabling WebSearch shows the globe.
func ToolSearch(args map[string]any, labelOf func(name string) Label) Label {
	query := String(args, "query")
	selected, isSelect := strings.CutPrefix(query, "select:")
	if !isSelect {
		what := Preview(query, 40, "tools")
		return New("wrench.and.screwdriver", "Finding "+what, "Found "+what, "Tool search attempted")
	}
	var names []string
	icon := ""
	for _, name := range strings.Split(selected, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		names = append(names, toolName(name))
		switch tool := labelOf(name).Icon; {
		case icon == "":
			icon = tool
		case icon != tool:
			icon = "wrench.and.screwdriver"
		}
	}
	what := "tools"
	if len(names) > 0 {
		what = strings.Join(names[:min(3, len(names))], ", ")
		if len(names) > 3 {
			what += fmt.Sprintf(" and %d more", len(names)-3)
		}
	}
	if icon == "" {
		icon = "wrench.and.screwdriver"
	}
	return New(icon, "Enabling "+what, "Enabled "+what, "Tool enable attempted")
}

// toolName is a tool's name as a reader says it: `WebSearch` is "Web Search",
// an MCP tool its action and server.
func toolName(name string) string {
	if label, ok := mcp(name); ok {
		return label.Labels.Completed
	}
	return humanizeToolName(name)
}

// EnterPlanMode starts planning before any edits.
func EnterPlanMode() Label {
	return New("list.bullet.clipboard", "Entering plan mode", "Planning the approach", "Plan mode attempted")
}

// ExitPlanMode hands the plan to the user for approval.
func ExitPlanMode() Label {
	return New("checkmark.circle", "Finishing the plan", "Presented plan for approval", "Plan attempted")
}

// fallback is a tool nothing else names: its name, humanized.
func fallback(name string) Label {
	label := humanizeToolName(name)
	return New("wrench.and.screwdriver", "Running "+label, "Ran "+label, label+" attempted")
}

// mcp labels `mcp__<server>__<tool>`: the tool humanized, qualified by its
// server. The server key is a slug of the user's own label, so it can't be
// listed; `mcp_stripe_com` reads as the host it came from with dots back.
func mcp(name string) (Label, bool) {
	parts := strings.Split(name, "__")
	if len(parts) < 3 || parts[0] != "mcp" || parts[1] == "" {
		return Label{}, false
	}
	tool := strings.Join(parts[2:], "__")
	if tool == "" {
		return Label{}, false
	}
	action := capitalize(strings.Join(strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(tool)), " "))
	source := strings.TrimPrefix(strings.ReplaceAll(parts[1], "_", "."), "mcp.")
	line := action
	if source != "" {
		line = action + " · " + source
	}
	return New("powerplug.fill", line, line, action+" attempted"), true
}

// Preview collapses whitespace runs to one space and cuts to max characters,
// marking the cut; empty text reads as fallback.
func Preview(text string, max int, fallback string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return fallback
	}
	if runes := []rune(text); len(runes) > max {
		return string(runes[:max]) + "..."
	}
	return text
}

// FileName is a path's last component, or fallback for an empty path.
func FileName(p, fallback string) string {
	if p == "" {
		return fallback
	}
	return path.Base(p)
}

// Host is a URL's host without its `www.`, or "" when ref is not a URL.
func Host(ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

// String is args[key] when it is a string, else "".
func String(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// Int is args[key] as a whole number, 0 when it is not one.
func Int(args map[string]any, key string) int {
	f, _ := args[key].(float64)
	return int(f)
}

// Dicts is args[key] as a list of objects, skipping anything else.
func Dicts(args map[string]any, key string) []map[string]any {
	return dicts(args[key])
}

func dicts(v any) []map[string]any {
	var out []map[string]any
	for _, item := range list(v) {
		if d, ok := item.(map[string]any); ok {
			out = append(out, d)
		}
	}
	return out
}

func list(v any) []any {
	items, _ := v.([]any)
	return items
}

func trimmed(args map[string]any, key string) string {
	return strings.TrimSpace(String(args, key))
}

// filePath is Claude's `file_path`, or `path` for the other agents.
func filePath(args map[string]any) string {
	if p := String(args, "file_path"); p != "" {
		return p
	}
	return String(args, "path")
}

// shortPath keeps a path's last two segments: `.../internal/store`.
func shortPath(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return "/"
	}
	parts := strings.Split(p, "/")
	if len(parts) <= 2 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

func capitalize(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// humanizeToolName splits on `_`, `-` and camelCase and capitalizes each word:
// `get_balance` and `getBalance` read "Get Balance"; empty reads "Tool".
func humanizeToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Tool"
	}
	var spaced strings.Builder
	prev := ' '
	for i, r := range name {
		if r == '_' || r == '-' {
			spaced.WriteRune(' ')
			prev = ' '
			continue
		}
		if i > 0 && prev != ' ' && r >= 'A' && r <= 'Z' {
			spaced.WriteRune(' ')
		}
		spaced.WriteRune(r)
		prev = r
	}
	words := strings.Fields(spaced.String())
	for i, w := range words {
		words[i] = capitalize(w)
	}
	return strings.Join(words, " ")
}
