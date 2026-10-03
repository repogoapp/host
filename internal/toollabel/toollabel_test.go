package toollabel

import (
	"encoding/json"
	"testing"
)

func args(t *testing.T, raw string) map[string]any {
	t.Helper()
	a, _ := Input(json.RawMessage(raw))
	return a
}

// Input reads an object, a string holding one (Codex's function arguments),
// and a custom tool's raw text.
func TestInputReadsEveryShape(t *testing.T) {
	a, text := Input(json.RawMessage(`{"cmd":"ls"}`))
	if a["cmd"] != "ls" || text != "" {
		t.Errorf("object: %v %q", a, text)
	}
	a, text = Input(json.RawMessage(`"{\"cmd\":\"ls\"}"`))
	if a["cmd"] != "ls" || text != "" {
		t.Errorf("string holding an object: %v %q", a, text)
	}
	a, text = Input(json.RawMessage(`"*** Begin Patch"`))
	if a != nil || text != "*** Begin Patch" {
		t.Errorf("raw text: %v %q", a, text)
	}
	if a, text = Input(nil); a != nil || text != "" {
		t.Errorf("empty: %v %q", a, text)
	}
}

func TestSharedVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        Label
	}{
		{"read", `{"path":"/repo/internal/store/read.go"}`,
			New("doc.text.magnifyingglass", "Reading read.go", "Read read.go", "Reading files attempted")},
		{"read", `{}`,
			New("doc.text.magnifyingglass", "Reading files", "Read files", "Reading files attempted")},
		{"edit", `{"path":"a/b.swift"}`,
			New("square.and.pencil", "Editing b.swift", "Edited b.swift", "Editing file attempted")},
		{"delete", `{"path":"x.txt"}`,
			New("trash", "Deleting x.txt", "Deleted x.txt", "Deleting file attempted")},
		{"execute", `{"command":"go   test\n ./..."}`,
			New("terminal", "go test ./...", "go test ./...", "Command attempted")},
		{"execute", `{"command":["bash","-lc","make native"]}`,
			New("terminal", "make native", "make native", "Command attempted")},
		{"execute", `{}`,
			New("terminal", "command", "command", "Command attempted")},
		{"ls", `{"path":"/Users/me/repo/internal/store/"}`,
			New("folder", "Listing .../internal/store", "Listed .../internal/store", "Listing directory attempted")},
		{"ls", `{}`,
			New("folder", "Listing /", "Listed /", "Listing directory attempted")},
		{"search", `{"pattern":"func main"}`,
			New("magnifyingglass", "Searching func main", "Searched func main", "Grep search attempted")},
		{"glob", `{"pattern":"**/*.go"}`,
			New("doc.text.magnifyingglass", "Searching **/*.go", "Searched **/*.go", "Glob search attempted")},
		{"web_search", `{"query":"swift observation"}`,
			New("globe", "Searching swift observation", "Searched swift observation", "Web search attempted")},
		{"fetch", `{"url":"https://api.example.com/v1"}`,
			New("network", "GET https://api.example.com/v1", "Sent GET https://api.example.com/v1", "Request attempted")},
		{"task", `{"task_name":"/root/locate_versioning"}`,
			New("person.2", "Running Locate versioning", "Ran Locate versioning", "Subagent failed: Locate versioning")},
		{"browser", `{"action":"snapshot"}`,
			New("safari", "Reading the browser", "Read the browser", "Browser action failed")},
		{"browser", `{"action":"click"}`,
			New("safari", "Controlling the browser", "Controlled the browser", "Browser action failed")},
		{"think", `{}`, New("brain", "Thinking", "Thought", "Thinking failed")},
		{"mcp__mcp_stripe_com__get_balance_summary", `{}`,
			New("powerplug.fill", "Get balance summary · stripe.com", "Get balance summary · stripe.com", "Get balance summary attempted")},
		{"mcp__repogo__browser", `{}`,
			New("powerplug.fill", "Browser · repogo", "Browser · repogo", "Browser attempted")},
		{"getBalance_summary", `{}`,
			New("wrench.and.screwdriver", "Running Get Balance Summary", "Ran Get Balance Summary", "Get Balance Summary attempted")},
		{"", `{}`,
			New("wrench.and.screwdriver", "Running Tool", "Ran Tool", "Tool attempted")},
	} {
		if got := Shared(tc.name, args(t, tc.input)); got != tc.want {
			t.Errorf("%s %s:\n got %+v\nwant %+v", tc.name, tc.input, got, tc.want)
		}
	}
}

// A malformed MCP name is an ordinary tool, not an empty server or tool.
func TestMCPNeedsServerAndTool(t *testing.T) {
	for _, name := range []string{"mcp__", "mcp__server__", "mcp____tool"} {
		if got := Shared(name, nil); got.Icon != "wrench.and.screwdriver" {
			t.Errorf("%q labelled as MCP: %+v", name, got)
		}
	}
}

func TestPreview(t *testing.T) {
	for _, tc := range []struct {
		text     string
		max      int
		fallback string
		want     string
	}{
		{"  a \n\t b  ", 40, "x", "a b"},
		{"", 40, "fallback", "fallback"},
		{"  \n ", 40, "fallback", "fallback"},
		{"abcdef", 3, "", "abc..."},
		{"héllo wörld", 5, "", "héllo..."},
	} {
		if got := Preview(tc.text, tc.max, tc.fallback); got != tc.want {
			t.Errorf("Preview(%q, %d) = %q, want %q", tc.text, tc.max, got, tc.want)
		}
	}
}

// A subagent's title: its description, a task name humanized, else the first
// line of its prompt, cut at 60 characters.
func TestSubagentTitle(t *testing.T) {
	long := "Find every place the relay forwards bytes and check each is sealed end to end"
	for _, tc := range []struct{ input, want string }{
		{`{"description":"  Audit the relay "}`, "Audit the relay"},
		{`{"task_name":"fix-login_flow"}`, "Fix login flow"},
		{`{"prompt":"First line\nsecond line"}`, "First line"},
		{`{"message":"Keep going"}`, "Keep going"},
		{`{"prompt":"` + long + `"}`, long[:59] + "…"},
		{`{}`, ""},
	} {
		if got := subagentTitle(args(t, tc.input)); got != tc.want {
			t.Errorf("subagentTitle(%s) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestTodosCount(t *testing.T) {
	if got := Todos(args(t, `{"todos":[{"content":"a"}]}`)).Labels.Completed; got != "Updated 1 todo" {
		t.Errorf("one: %q", got)
	}
	if got := Todos(args(t, `{"todos":[{},{},{}]}`)).Labels.Completed; got != "Updated 3 todos" {
		t.Errorf("three: %q", got)
	}
	if got := Todos(nil).Labels.Completed; got != "Updated 0 todos" {
		t.Errorf("none: %q", got)
	}
}

// A question reads as its header; an empty header still wins over the
// question, and reads as "Question".
func TestQuestionTitle(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`{"questions":[{"header":"Auth method","question":"Which one?"}]}`, "Auth method"},
		{`{"questions":[{"question":"Which one?"}]}`, "Which one?"},
		{`{"questions":[{"header":"","question":"Which one?"}]}`, "Question"},
		{`{}`, "Question"},
	} {
		if got := Question(args(t, tc.input)).Labels.Active; got != tc.want {
			t.Errorf("Question(%s) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestToolSearch(t *testing.T) {
	labelOf := func(name string) Label { return Shared(name, nil) }
	for _, tc := range []struct{ input, icon, want string }{
		{`{"query":"select:webSearch"}`, "globe", "Enabled Web Search"},
		{`{"query":"select:webSearch,webFetch"}`, "wrench.and.screwdriver", "Enabled Web Search, Web Fetch"},
		{`{"query":"select:mcp__neon__run_sql"}`, "powerplug.fill", "Enabled Run sql · neon"},
		{`{"query":"select:A, B,C,D,E"}`, "wrench.and.screwdriver", "Enabled A, B, C and 2 more"},
		{`{"query":"slack send"}`, "wrench.and.screwdriver", "Found slack send"},
		{`{}`, "wrench.and.screwdriver", "Found tools"},
	} {
		got := ToolSearch(args(t, tc.input), labelOf)
		if got.Icon != tc.icon || got.Labels.Completed != tc.want {
			t.Errorf("ToolSearch(%s) = %q %q, want %q %q", tc.input, got.Icon, got.Labels.Completed, tc.icon, tc.want)
		}
	}
}

// The wire shape is an icon and a line per state.
func TestLabelJSON(t *testing.T) {
	b, err := json.Marshal(New("terminal", "a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"icon":"terminal","labels":{"active":"a","completed":"b","error":"c"}}`; string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}
