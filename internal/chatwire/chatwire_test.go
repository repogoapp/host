package chatwire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/toollabel"
)

// fake labels every named call "Ran <name>", so the tests see what the
// builder adds around a provider's row.
type fake struct{ kind agent.Kind }

func (f fake) Kind() agent.Kind { return f.kind }

// Resolve names an "exec" call for the one tool it runs, as Codex's cells.
func (f fake) Resolve(call agent.ToolCall) agent.ToolCall {
	if call.Name == "exec" {
		call.Name, call.Input = "exec_command", json.RawMessage(`{"cmd":"ls"}`)
	}
	return call
}

func (f fake) ToolRow(call agent.ToolCall) Tool {
	if call.Name == "" {
		return Tool{}
	}
	args, _ := toollabel.Input(call.Input)
	return Labeled(call.Name, toollabel.New("terminal", "Running "+call.Name, "Ran "+call.Name, "Failed"), args)
}

func toolRow(t *testing.T, kind string, call agent.ToolCall) store.Message {
	t.Helper()
	b, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	return store.Message{Kind: kind, Tool: string(b)}
}

func TestPageCarriesLabelsNotPayloads(t *testing.T) {
	b := New([]Labeler{fake{"claude"}})
	page := b.Page(store.Page{
		ChatID: "claude:s1", HostID: "h", Cwd: "/repo", Agent: "claude", Generation: 3,
		EventCount: 9, NextIdx: 9, FirstIdx: 6, HasBefore: true,
		Events: []store.Message{
			{Idx: 6, Kind: "text", Turn: "t1", Text: "Looking.", At: 100},
			toolRow(t, "tool_call", agent.ToolCall{CallID: "c1", Name: "Read",
				Input: json.RawMessage(`{"file_path":"/repo/secret/main.go"}`)}),
			toolRow(t, "tool_result", agent.ToolCall{CallID: "c1", Output: "package main // the file's contents"}),
		},
	})

	want := Page{
		ChatID: "claude:s1", HostID: "h", Cwd: "/repo", Agent: "claude", Generation: 3,
		EventCount: 9, NextIdx: 9, FirstIdx: 6, HasBefore: true,
		Events: []Message{
			{Idx: 6, Kind: "text", Turn: "t1", Text: "Looking.", At: 100},
			{Kind: "tool_call", Tool: &Tool{CallID: "c1", Name: "Read", State: StateRunning, Icon: "terminal",
				Labels: &toollabel.Lines{Active: "Running Read", Completed: "Ran Read", Error: "Failed"}, File: "main.go"}},
			{Kind: "tool_result", Tool: &Tool{CallID: "c1", State: StateCompleted}},
		},
	}
	if !reflect.DeepEqual(page, want) {
		t.Fatalf("got %+v\nwant %+v", page, want)
	}

	// Neither the input nor the output leaves the host.
	wire, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"secret/main.go", "file's contents", `"input"`, `"output"`} {
		if strings.Contains(string(wire), leak) {
			t.Errorf("page carries %q: %s", leak, wire)
		}
	}
}

// A call row is running until it carries output; a result row is done; an
// error from either is failed.
func TestState(t *testing.T) {
	for _, tc := range []struct {
		kind string
		call agent.ToolCall
		want State
	}{
		{"tool_call", agent.ToolCall{Name: "Bash"}, StateRunning},
		{"tool_call", agent.ToolCall{Name: "Bash", Output: "ok"}, StateCompleted},
		{"tool_call", agent.ToolCall{Name: "Bash", Output: "boom", IsError: true}, StateFailed},
		{"tool_result", agent.ToolCall{}, StateCompleted},
		{"tool_result", agent.ToolCall{Output: "denied", IsError: true}, StateFailed},
	} {
		got := New(nil).Messages("claude", []store.Message{toolRow(t, tc.kind, tc.call)})[0].Tool.State
		if got != tc.want {
			t.Errorf("%s %+v: got %s, want %s", tc.kind, tc.call, got, tc.want)
		}
	}
}

// An agent this host doesn't register still gets the shared vocabulary.
func TestUnknownAgentGetsSharedLabels(t *testing.T) {
	rows := New(nil).Messages("gemini", []store.Message{
		toolRow(t, "tool_call", agent.ToolCall{CallID: "c", Name: "read", Input: json.RawMessage(`{"path":"a/b.go"}`)}),
		toolRow(t, "tool_result", agent.ToolCall{CallID: "c", Output: "x"}),
	})
	call := rows[0].Tool
	if call.Name != "read" || call.Icon != "doc.text.magnifyingglass" || call.Labels.Completed != "Read b.go" || call.File != "b.go" {
		t.Errorf("call row: %+v %+v", call, call.Labels)
	}
	if result := rows[1].Tool; result.Labels != nil || result.Name != "" {
		t.Errorf("an unnamed result row was labelled: %+v", result)
	}
}

// A browser call's recording comes from its output, on whichever row
// carries it.
func TestRecordingID(t *testing.T) {
	rows := New(nil).Messages("claude", []store.Message{
		toolRow(t, "tool_result", agent.ToolCall{CallID: "c", Output: `{"ok":true,"recordingId":"rec-1"}`}),
		toolRow(t, "tool_result", agent.ToolCall{CallID: "d", Output: "no recording here"}),
		toolRow(t, "tool_result", agent.ToolCall{CallID: "e", Output: "recordingId mentioned in prose"}),
		toolRow(t, "tool_result", agent.ToolCall{CallID: "f", Output: "Script completed\nWall time 0.3 seconds\nOutput:\n" +
			`{"content":[{"type":"text","text":"{\n  \"action\": \"navigate\",\n  \"recordingId\": \"rec-2\"\n}"}]}`}),
	})
	for i, want := range []string{"rec-1", "", "", "rec-2"} {
		if got := rows[i].Tool.RecordingID; got != want {
			t.Errorf("row %d: recording %q, want %q", i, got, want)
		}
	}
}

// A row whose stored tool isn't JSON keeps its place without a tool.
func TestMalformedToolIsDropped(t *testing.T) {
	rows := New(nil).Messages("claude", []store.Message{{Idx: 4, Kind: "tool_call", Tool: "{not json"}})
	if rows[0].Tool != nil || rows[0].Idx != 4 {
		t.Errorf("got %+v", rows[0])
	}
}

func TestTodos(t *testing.T) {
	args := map[string]any{"todos": []any{
		map[string]any{"content": "Port labels", "status": "completed", "id": "a"},
		map[string]any{"text": "  Write tests  ", "status": "in_progress"},
		map[string]any{"status": "pending"},
		"not an item",
	}}
	want := []Todo{
		{ID: "a", Content: "Port labels", Status: "completed"},
		{ID: "1-Write tests", Content: "Write tests", Status: "in_progress"},
		{ID: "2-Todo 3", Content: "Todo 3", Status: "pending"},
	}
	if got := Todos("Todos have been modified successfully.", args); !reflect.DeepEqual(got, want) {
		t.Errorf("from input: got %+v\nwant %+v", got, want)
	}

	// Codex's update_plan: `plan: [{step, status}]`; a list in the output wins.
	plan := map[string]any{"plan": []any{map[string]any{"step": "Read", "status": "completed"}}}
	if got := Todos("", plan); !reflect.DeepEqual(got, []Todo{{ID: "0-Read", Content: "Read", Status: "completed"}}) {
		t.Errorf("plan: %+v", got)
	}
	if got := Todos(`{"items":[{"title":"From output"}]}`, plan); len(got) != 1 || got[0].Content != "From output" {
		t.Errorf("output first: %+v", got)
	}
	if got := Todos("", nil); got != nil {
		t.Errorf("none: %+v", got)
	}
}

func TestFile(t *testing.T) {
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"file_path": "/repo/apps/App.swift"}, "App.swift"},
		{map[string]any{"filePath": "a/b.ts"}, "b.ts"},
		{map[string]any{"path": "  "}, ""},
		{map[string]any{"path": " ", "file": "notes.md"}, "notes.md"},
		{map[string]any{"command": "ls"}, ""},
	} {
		if got := fileName(tc.args); got != tc.want {
			t.Errorf("fileName(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// A sheet's call is its rows merged: name and input from the call, output
// and state from the result; the agent resolves what the call ran.
func TestDetails(t *testing.T) {
	b := New([]Labeler{fake{"codex"}})
	rows := []store.Message{
		toolRow(t, "tool_call", agent.ToolCall{CallID: "a", Name: "Read", Input: json.RawMessage(`{"file_path":"go.mod"}`)}),
		toolRow(t, "tool_call", agent.ToolCall{CallID: "b", Name: "Bash", Input: json.RawMessage(`{"command":"make"}`)}),
		toolRow(t, "tool_result", agent.ToolCall{CallID: "a", Output: "module x"}),
		toolRow(t, "tool_call", agent.ToolCall{CallID: "c", Name: "Bash", Output: "boom", IsError: true}),
		toolRow(t, "tool_call", agent.ToolCall{CallID: "d", Name: "exec", Input: json.RawMessage(`"script"`)}),
		toolRow(t, "tool_result", agent.ToolCall{Output: "no call id"}),
	}
	want := []ToolDetail{
		{CallID: "a", Name: "Read", State: StateCompleted, Input: json.RawMessage(`{"file_path":"go.mod"}`), Output: "module x"},
		{CallID: "b", Name: "Bash", State: StateRunning, Input: json.RawMessage(`{"command":"make"}`)},
		{CallID: "c", Name: "Bash", State: StateFailed, Output: "boom"},
		{CallID: "d", Name: "exec_command", State: StateRunning, Input: json.RawMessage(`{"cmd":"ls"}`)},
	}
	if got := b.Details("codex", rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// A long output is cut to MaxOutput, whole characters only, and says how
// long it was.
func TestDetailOutputIsCut(t *testing.T) {
	long := strings.Repeat("é", MaxOutput) // two bytes each
	got := New(nil).Details("claude", []store.Message{
		toolRow(t, "tool_result", agent.ToolCall{CallID: "a", Output: long}),
	})[0]
	if got.OutputBytes != len(long) || len(got.Output) != MaxOutput || !utf8.ValidString(got.Output) {
		t.Errorf("output %d bytes (valid %v), output_bytes %d", len(got.Output), utf8.ValidString(got.Output), got.OutputBytes)
	}
}
