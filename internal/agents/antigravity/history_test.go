package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

// conversationAt writes an agy conversation's log and lists it in agy's
// metadata, in folder cwd.
func conversationAt(t *testing.T, p *Provider, meta map[string]string, id, cwd, log string) {
	t.Helper()
	dir := filepath.Join(p.home, "brain", id, ".system_generated", "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, usageLog), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	meta[id] = cwd
	var entries []string
	for k, v := range meta {
		entries = append(entries, `"`+k+`":{"summary":{"Title":"T `+k+`","WorkspaceURIs":["file://`+v+`"]}}`)
	}
	_ = os.MkdirAll(filepath.Join(p.home, "cache"), 0o700)
	if err := os.WriteFile(filepath.Join(p.home, "cache", "conversation_metadata.json"), []byte(`{"conversations":{`+strings.Join(entries, ",")+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

const sampleLog = `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-22T06:13:07Z","content":"<USER_REQUEST>\nList the files\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\ntime\n</ADDITIONAL_METADATA>"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-22T06:13:08Z","model":"gemini-4-neon-high","input_tokens":10,"output_tokens":5,"thinking":"Look first.","tool_calls":[{"name":"run_command","args":{"CommandLine":"ls -1"}},{"name":"read_file","args":{"Path":"/w/README.md"}}]}
{"step_index":2,"source":"MODEL","type":"GENERIC","status":"RUNNING","created_at":"2026-11-22T06:13:09Z","content":"Created At: x\npartial"}
{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-22T06:13:10Z","content":"Created At: x\nCompleted At: y\nREADME.md"}
{"step_index":3,"source":"MODEL","type":"GENERIC","status":"ERROR","created_at":"2026-11-22T06:13:11Z","content":"","error":"no such file"}
{"step_index":4,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","created_at":"2026-11-22T06:13:12Z","content":"system"}
{"step_index":5,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-22T06:13:13Z","model":"gemini-4-neon-high","input_tokens":10,"output_tokens":5,"content":"One file."}
`

func TestTerminalChatsAreListedAndRead(t *testing.T) {
	p := New(agent.Dependencies{Root: t.TempDir()})
	defer p.Close()
	work, gone := t.TempDir(), filepath.Join(t.TempDir(), "gone")
	meta := map[string]string{}
	conversationAt(t, p, meta, "terminal", work, sampleLog)
	conversationAt(t, p, meta, "orphan", gone, sampleLog)
	// RepoGo's own chat names its agy conversation, which isn't listed again.
	conversationAt(t, p, meta, "repogos", work, sampleLog)
	if _, err := p.run.Chats().Adopt("repogos", work); err != nil {
		t.Fatal(err)
	}
	h := p.Sessions()
	metas, err := h.List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range metas {
		ids = append(ids, m.ID+"@"+filepath.Base(m.Path))
	}
	slices.Sort(ids)
	// "repogos" was adopted under its own id, so it reads from agy's log; the
	// orphan's folder is gone.
	if strings.Join(ids, " ") != "repogos@transcript_full.jsonl terminal@transcript_full.jsonl" {
		t.Fatalf("listed %v", ids)
	}

	var terminal session.Meta
	for _, m := range metas {
		if m.ID == "terminal" {
			terminal = m
		}
	}
	if terminal.Cwd != work || terminal.Title != "T terminal" {
		t.Fatalf("meta %+v", terminal)
	}
	events, err := session.ReadAll(terminal, h)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		row := string(e.Kind) + "[" + e.TurnID + "]"
		switch {
		case e.Tool != nil:
			row += e.Tool.CallID + "=" + e.Tool.Name + ":" + e.Tool.Output
		default:
			row += e.Text
		}
		got = append(got, row)
	}
	want := []string{
		"user_message[agy-0]List the files",
		"reasoning[agy-0]Look first.",
		"tool_call[agy-0]agy-1-0=run_command:",
		"tool_call[agy-0]agy-1-1=read_file:",
		"tool_result[agy-0]agy-1-0=run_command:README.md",
		"tool_result[agy-0]agy-1-1=read_file:no such file",
		"text[agy-0]One file.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events\n%s", strings.Join(got, "\n"))
	}

	// A rename is kept in the host's file, never agy's log.
	if err := h.(session.Renamer).Rename(terminal, "Mine"); err != nil {
		t.Fatal(err)
	}
	metas, _ = h.List()
	for _, m := range metas {
		if m.ID == "terminal" && (m.Title != "Mine" || filepath.Base(m.Path) != usageLog) {
			t.Fatalf("renamed %+v", m)
		}
	}
	if raw, _ := os.ReadFile(terminal.Path); string(raw) != sampleLog {
		t.Fatal("agy's log was written to")
	}
}

// A watched chat's results find their calls as agy writes them, a step's
// second reading updating the same call.
func TestStreamingResultsFindTheirCalls(t *testing.T) {
	p := New(agent.Dependencies{Root: t.TempDir()})
	defer p.Close()
	lines := strings.SplitAfter(sampleLog, "\n")
	// The store hides chats in temporary folders, so this one runs here.
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	conversationAt(t, p, map[string]string{}, "live", here, strings.Join(lines[:3], ""))
	store := session.NewStore(p.Sessions())
	if _, err := store.List(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, events, err := store.Watch(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(p.home, "brain", "live", ".system_generated", "logs", usageLog)
	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var results []string
	for e := range events {
		if e.Kind == agent.EventToolCall && e.Tool.CallID == "agy-1-1" {
			// The replay is out; agy writes the rest.
			if _, err := f.WriteString(strings.Join(lines[3:], "")); err != nil {
				t.Fatal(err)
			}
		}
		if e.Kind == agent.EventToolResult {
			results = append(results, e.Tool.CallID+"="+e.Tool.Output)
		}
		if e.Kind == agent.EventText {
			break
		}
	}
	want := "agy-1-0=partial agy-1-0=README.md agy-1-1=no such file"
	if got := strings.Join(results, " "); got != want {
		t.Fatalf("results %q, want %q", got, want)
	}
}
