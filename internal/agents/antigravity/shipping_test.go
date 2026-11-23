package antigravity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/shipping"
)

func TestModelCallsAreReadFromTheFullLogOnly(t *testing.T) {
	root := t.TempDir()
	p := New(agent.Dependencies{Root: root})
	defer p.Close()
	logs := filepath.Join(p.home, "brain", "conv-1", ".system_generated", "logs")
	if err := os.MkdirAll(filepath.Join(p.home, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"conversations":{"conv-1":{"summary":{"WorkspaceURIs":["file:///work/app","file:///private/work/app"]}}}}`
	if err := os.WriteFile(filepath.Join(p.home, "cache", "conversation_metadata.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	parse := p.NewFileUsageParser(filepath.Join(logs, "transcript_full.jsonl"))
	got := parse([]byte(`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-22T05:26:58Z","model":"gemini-4-neon-high","input_tokens":2359,"cache_read_tokens":6548,"output_tokens":216,"content":"done"}`))
	want := shipping.Record{Key: "conv-1:3", Session: "conv-1", Cwd: "/work/app", TimestampMs: 1795325218000, Model: "gemini-4-neon",
		Tokens: shipping.Tokens{Uncached: 2359, Cached: 6548, Output: 216}}
	if got == nil || *got != want {
		t.Fatalf("got %+v", got)
	}
	if r := parse([]byte(`{"step_index":4,"type":"PLANNER_RESPONSE","created_at":"2026-11-22T05:26:58Z","model":"claude-opus-4-6-thinking","input_tokens":1}`)); r == nil || r.Model != "claude-opus-4-6" {
		t.Errorf("thinking model %+v", r)
	}
	if parse([]byte(`{"step_index":0,"type":"USER_INPUT","created_at":"2026-11-22T05:26:58Z","content":"PLANNER_RESPONSE"}`)) != nil {
		t.Error("a prompt billed")
	}
	// The copies beside the full log repeat its steps.
	for _, copy := range []string{filepath.Join(logs, "transcript.jsonl"), filepath.Join(logs, "chunks", "transcript", "00000001.jsonl")} {
		if p.NewFileUsageParser(copy)([]byte(`{"step_index":3,"type":"PLANNER_RESPONSE","created_at":"2026-11-22T05:26:58Z","model":"m","input_tokens":1}`)) != nil {
			t.Errorf("%s billed", filepath.Base(copy))
		}
	}
	if roots := p.UsageRoots(); len(roots) != 1 || filepath.Dir(roots[0]) != p.Definition().Tool.Home {
		t.Errorf("roots %v", roots)
	}
}
