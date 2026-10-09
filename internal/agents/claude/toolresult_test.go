package claude

import (
	"bufio"
	"os"
	"reflect"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

// The fixture's last five lines carry the CLI's toolUseResult in the shapes it
// writes: a shell run with stderr, a failed run, a Write, a search, a fetch.
func TestClaudeReadsToolUseResult(t *testing.T) {
	two := 2
	want := map[string]struct {
		output string
		result *agent.ToolResult
	}{
		"c1": {"README.md", nil},
		"c2": {"built", &agent.ToolResult{TimedOut: true, Stderr: "warning: slow"}},
		"c3": {"Exit code 2\nno such file", &agent.ToolResult{ExitCode: &two}},
		"c4": {"File created", &agent.ToolResult{Created: true}},
		"c5": {"Web search results", &agent.ToolResult{DurationMS: 1500, URLs: []agent.ToolURL{{Title: "Go", URL: "https://go.dev"}}}},
		"c6": {"page", &agent.ToolResult{DurationMS: 2341, HTTPStatus: 200}},
	}
	f, err := os.Open("testdata/transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	seen := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		for _, e := range session.Parse(&Sessions{}, scanner.Bytes()) {
			if e.Kind != agent.EventToolResult {
				continue
			}
			w, ok := want[e.Tool.CallID]
			if !ok {
				t.Fatalf("unexpected result %q", e.Tool.CallID)
			}
			seen++
			if e.Tool.Output != w.output {
				t.Errorf("%s output: got %q, want %q", e.Tool.CallID, e.Tool.Output, w.output)
			}
			if !reflect.DeepEqual(e.Tool.Result, w.result) {
				t.Errorf("%s result: got %+v, want %+v", e.Tool.CallID, e.Tool.Result, w.result)
			}
		}
	}
	if seen != len(want) {
		t.Errorf("saw %d results, want %d", seen, len(want))
	}
}
