package liveactivity

import (
	"cmp"
	"encoding/json"
	"path"
	"strconv"
	"strings"

	"github.com/repogo/host/internal/notify"
)

// The lock screen's words for a tool call, from Claude's hook tool names. The
// text and icons are v1's (`apps/spine/internal/agent/agentevent`), so a
// running turn reads the way it did there.

type icon struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	ActiveColor string `json:"activeColor"`
}

type label struct {
	Title, Subtitle, Error string
	Active, Completed      string
	Icon                   icon
}

const inactiveColor = "#6b7280"

func ic(name, activeColor string) icon {
	return icon{Name: name, Color: inactiveColor, ActiveColor: activeColor}
}

var (
	iconRead       = ic("doc.text.magnifyingglass", "#3b82f6")
	iconWrite      = ic("square.and.pencil", "#3b82f6")
	iconReplace    = ic("arrow.left.arrow.right", "#3b82f6")
	iconShell      = ic("terminal", "#f97316")
	iconGlob       = ic("doc.text.magnifyingglass", "#8b5cf6")
	iconGrep       = ic("magnifyingglass", "#06b6d4")
	iconLs         = ic("folder", "#f59e0b")
	iconQuestion   = ic("questionmark.bubble", "#2563eb")
	iconWebSearch  = ic("globe", "#ec4899")
	iconWebFetch   = ic("arrow.down.circle", "#3b82f6")
	iconAgent      = ic("sparkles", "#8b5cf6")
	iconChecklist  = ic("checklist", "#22c55e")
	iconResources  = ic("list.bullet", "#8b5cf6")
	iconToolSearch = ic("wrench.and.screwdriver", "#10b981")
	iconSkill      = ic("arrow.down.doc", "#8b5cf6")
	iconMCP        = ic("powerplug.fill", "#0ea5e9")
	genericIcon    = ic("wrench.and.screwdriver", inactiveColor)
)

// labelFor names a Claude tool call from its hook input.
func labelFor(tool string, raw json.RawMessage) label {
	in := map[string]any{}
	_ = json.Unmarshal(raw, &in)
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := in[k].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
		return ""
	}
	count := func(key string) int {
		list, _ := in[key].([]any)
		return len(list)
	}

	switch tool {
	case "Read":
		p := str("file_path", "path")
		return label{Title: "Reading " + fileName(p), Subtitle: cmp.Or(p, fileName(p)), Error: "Reading files attempted",
			Active: "Reading file", Completed: "Read file", Icon: iconRead}
	case "Write", "NotebookEdit":
		p := str("file_path", "notebook_path", "path")
		return label{Title: "Editing " + fileName(p), Subtitle: p, Error: "Editing file attempted",
			Active: "Editing file", Completed: "Edited file", Icon: iconWrite}
	case "Edit", "MultiEdit":
		p := str("file_path", "path")
		return label{Title: "Replacing " + fileName(p), Subtitle: p, Error: "Replacing file attempted",
			Active: "Replacing file", Completed: "Replaced file", Icon: iconReplace}
	case "Bash":
		// Claude describes the command; the raw command is the fallback.
		return label{Title: preview(str("description", "command"), 40, "command"), Subtitle: "Shell command",
			Error: "Command attempted", Active: "Running command", Completed: "Ran command", Icon: iconShell}
	case "Glob":
		return label{Title: "Searching " + preview(str("pattern"), 40, "files"), Subtitle: "File pattern",
			Error: "Glob search attempted", Active: "Searching files", Completed: "Searched files", Icon: iconGlob}
	case "Grep":
		return label{Title: "Searching " + preview(str("pattern"), 40, "code"), Subtitle: "Code in " + shortPath(str("path")),
			Error: "Grep search attempted", Active: "Searching code", Completed: "Searched code", Icon: iconGrep}
	case "LS":
		p := "/"
		if raw := str("path"); raw != "" && raw != "/" {
			p = shortPath(raw)
		}
		return label{Title: "Listing " + p, Subtitle: "Directory contents", Error: "Listing directory attempted",
			Active: "Listing directory", Completed: "Listed directory", Icon: iconLs}
	case "WebSearch":
		return label{Title: "Searching " + preview(str("query"), 40, "web"), Subtitle: "Web search",
			Error: "Web search attempted", Active: "Searching web", Completed: "Searched web", Icon: iconWebSearch}
	case "WebFetch":
		return label{Title: "Fetching " + preview(str("url"), 50, "URL"), Subtitle: "Web page",
			Error: "Fetch attempted", Active: "Fetching URL", Completed: "Fetched URL", Icon: iconWebFetch}
	case "Task", "Agent":
		return label{Title: "Running " + preview(str("description"), 40, "agent"), Subtitle: str("subagent_type"),
			Error: "Sub-agent attempted", Active: "Running sub-agent", Completed: "Ran sub-agent", Icon: iconAgent}
	case "TodoWrite":
		return label{Title: "Updating " + plural(count("todos"), "todo"), Subtitle: "Task list",
			Error: "Updating todos attempted", Active: "Updating todos", Completed: "Updated todos", Icon: iconChecklist}
	case "TaskCreate":
		return label{Title: "Creating task: " + preview(str("subject"), 48, "task"), Subtitle: "Task list",
			Error: "Task creation attempted", Active: "Creating task", Completed: "Created task", Icon: iconChecklist}
	case "TaskUpdate":
		id := str("taskId")
		task := "#" + id
		if subject := str("subject"); subject != "" {
			task = preview(subject, 42, id)
		}
		return label{Title: "Updating task " + task, Subtitle: cmp.Or(str("status"), "Task list"),
			Error: "Task update attempted", Active: "Updating task", Completed: "Updated task", Icon: iconChecklist}
	case "AskUserQuestion":
		q := questionText(raw)
		return label{Title: preview(q, 48, "Question"), Subtitle: preview(q, 72, "Waiting for your answer"),
			Error: "Question attempted", Active: "Waiting for your input", Completed: "Answered question", Icon: iconQuestion}
	case "ListMcpResourcesTool":
		title, subtitle := "Listing MCP resources", "Available MCP resources"
		if server := preview(str("server"), 30, ""); server != "" {
			title, subtitle = "Listing "+server, "MCP resources"
		}
		return label{Title: title, Subtitle: subtitle, Error: "Listing MCP resources attempted",
			Active: "Listing resources", Completed: "Listed resources", Icon: iconResources}
	case "ToolSearch":
		title := "Enabling tools"
		if q := strings.TrimPrefix(str("query"), "select:"); q != "" {
			title = "Enabling " + preview(strings.ReplaceAll(q, ",", ", "), 40, "tools")
		}
		return label{Title: title, Subtitle: "Tool discovery", Error: "Tool enable attempted",
			Active: "Enabling tools", Completed: "Enabled tools", Icon: iconToolSearch}
	case "Skill":
		return label{Title: "Loading skill: " + preview(str("skill", "command"), 40, "skill"), Subtitle: "SKILL.md",
			Error: "Skill load attempted", Active: "Loading skill", Completed: "Loaded skill", Icon: iconSkill}
	case "EnterPlanMode":
		return label{Title: "Entering plan mode", Subtitle: "Read-only exploration", Error: "Plan mode attempted",
			Active: "Entering plan mode", Completed: "Planning the approach", Icon: ic("list.bullet.clipboard", "#8b5cf6")}
	case "ExitPlanMode":
		return label{Title: "Finishing the plan", Subtitle: "Plan for approval", Error: "Plan attempted",
			Active: "Finishing the plan", Completed: "Presented plan for approval", Icon: ic("checkmark.circle", "#22c55e")}
	}
	if server, action, ok := mcpTool(tool); ok {
		return label{Title: action, Subtitle: server, Error: action + " attempted",
			Active: action, Completed: action, Icon: iconMCP}
	}
	human := humanize(tool)
	return label{Title: "Running " + human, Subtitle: "Tool call", Error: human + " attempted",
		Active: "Running " + human, Completed: "Ran " + human, Icon: genericIcon}
}

// mcpTool splits `mcp__<server>__<tool>`; the server is a slug of the user's
// own label, so `mcp_stripe_com` reads as the host it came from.
func mcpTool(name string) (server, action string, ok bool) {
	rest, found := strings.CutPrefix(name, "mcp__")
	if !found {
		return "", "", false
	}
	server, tool, found := strings.Cut(rest, "__")
	if !found || server == "" || tool == "" {
		return "", "", false
	}
	return strings.TrimPrefix(strings.ReplaceAll(server, "_", "."), "mcp."), humanize(tool), true
}

var presentToPast = map[string]string{
	"Reading": "Read", "Editing": "Edited", "Writing": "Wrote", "Replacing": "Replaced",
	"Inserting": "Inserted", "Deleting": "Deleted", "Searching": "Searched", "Listing": "Listed",
	"Fetching": "Fetched", "Analyzing": "Analyzed", "Exploring": "Explored", "Running": "Ran",
	"Creating": "Created", "Updating": "Updated", "Loading": "Loaded", "Enabling": "Enabled",
	"Checking": "Checked", "Controlling": "Controlled", "Sending": "Sent", "Viewing": "Viewed",
}

// completedTitle puts a title's leading verb in the past tense ("Replacing
// README.md" → "Replaced README.md"); a raw command or a question is unchanged.
func completedTitle(title string) string {
	verb, rest, found := strings.Cut(title, " ")
	if past, ok := presentToPast[verb]; found && ok {
		return past + " " + rest
	}
	return title
}

func preview(s string, max int, fallback string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return fallback
	}
	if r := []rune(s); len(r) > max {
		return strings.TrimSpace(string(r[:max])) + "..."
	}
	return s
}

func fileName(p string) string {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return "file"
	}
	return path.Base(p)
}

func shortPath(p string) string {
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	switch {
	case len(parts) == 0:
		return "/"
	case len(parts) > 2:
		return ".../" + strings.Join(parts[len(parts)-2:], "/")
	}
	return strings.Join(parts, "/")
}

func plural(n int, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return strconv.Itoa(n) + " " + noun
}

// humanize turns `web_fetch` or `webFetch` into "Web Fetch".
func humanize(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Tool"
	}
	var b strings.Builder
	var prev rune
	for i, r := range name {
		if r == '_' || r == '-' {
			b.WriteRune(' ')
			prev = ' '
			continue
		}
		if i > 0 && prev != ' ' && r >= 'A' && r <= 'Z' {
			b.WriteRune(' ')
		}
		b.WriteRune(r)
		prev = r
	}
	words := strings.Fields(b.String())
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// lineChanges approximates an edit's "+A -B": the lines that differ once the
// lines both sides share at the start and end are set aside.
func lineChanges(tool string, raw json.RawMessage) (added, removed int) {
	var in struct {
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Edits     []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return 0, 0
	}
	switch tool {
	case "Write":
		return len(lines(in.Content)), 0
	case "Edit":
		return diffLines(in.OldString, in.NewString)
	case "MultiEdit":
		for _, e := range in.Edits {
			a, r := diffLines(e.OldString, e.NewString)
			added, removed = added+a, removed+r
		}
	}
	return added, removed
}

func diffLines(before, after string) (added, removed int) {
	a, b := lines(before), lines(after)
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	return len(b), len(a)
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// questionText is the first question a question tool's input asks.
func questionText(raw json.RawMessage) string {
	if qs := notify.Questions(raw); len(qs) > 0 {
		return qs[0].Text
	}
	return ""
}
