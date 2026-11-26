package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

// TestLiveAgy runs the installed, signed-in agy: the catalog, a turn, a tool
// it asks about, and a message steered into a turn. REPOGO_AGY_LIVE=1 runs it.
func TestLiveAgy(t *testing.T) {
	if os.Getenv("REPOGO_AGY_LIVE") != "1" {
		t.Skip("REPOGO_AGY_LIVE=1 runs it against the installed agy")
	}
	p := New(agent.Dependencies{Root: t.TempDir(), Context: t.Context(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer p.Close()
	if err := p.Available(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	start := time.Now()
	catalog, err := p.Catalog(ctx)
	if err != nil || len(catalog.Models) == 0 || catalog.DefaultModel == "" {
		t.Fatalf("catalog %+v %v", catalog, err)
	}
	for _, m := range catalog.Models {
		t.Logf("model %s %q efforts %v (default %q) company %q", m.ID, m.Name, m.Efforts, m.DefaultEffort, m.Company)
	}
	t.Logf("catalog in %s", time.Since(start).Round(time.Millisecond))

	dir := t.TempDir()
	run := func(req agent.TurnRequest, turnID string, during func(chat string)) (string, []agent.Approval, string, agent.Result) {
		var mu sync.Mutex
		var text strings.Builder
		var asked []agent.Approval
		var chat string
		var once sync.Once
		io := agent.TurnIO{TurnID: turnID,
			Emit: func(e agent.Event) {
				mu.Lock()
				defer mu.Unlock()
				if e.Kind == agent.EventText {
					text.WriteString(e.Text)
					if during != nil {
						once.Do(func() { go during(agent.ChatID(agent.KindAntigravity, chat)) })
					}
				}
			},
			Session: func(id, _ string) { mu.Lock(); chat = id; mu.Unlock() },
			Ask: func(_ context.Context, a agent.Approval) (json.RawMessage, error) {
				mu.Lock()
				asked = append(asked, a)
				mu.Unlock()
				raw, _ := json.Marshal(a.Options[0].OptionID)
				return raw, nil
			},
		}
		result, err := p.Send(ctx, req, io)
		if err != nil {
			t.Fatalf("%s: %v", req.Prompt, err)
		}
		mu.Lock()
		defer mu.Unlock()
		return text.String(), asked, chat, result
	}

	text, _, chat, result := run(agent.TurnRequest{Cwd: dir, Agent: agent.KindAntigravity, Prompt: "Reply with the single word: pong"}, "t1", nil)
	t.Logf("pong: %q stop %q usage %+v", text, result.StopReason, result.Usage)
	if !strings.Contains(strings.ToLower(text), "pong") {
		t.Fatalf("text %q", text)
	}

	text, asked, _, _ := run(agent.TurnRequest{Cwd: dir, Agent: agent.KindAntigravity, SessionID: chat,
		Prompt: "Create a file named hi.txt containing hi, then say done."}, "t2", nil)
	if len(asked) == 0 {
		t.Fatalf("the edit wasn't asked about; text %q", text)
	}
	t.Logf("asked %q kind %q options %v", asked[0].Title, asked[0].Kind, asked[0].Options)
	if raw, err := os.ReadFile(filepath.Join(dir, "hi.txt")); err != nil || strings.TrimSpace(string(raw)) != "hi" {
		t.Fatalf("hi.txt %q %v", raw, err)
	}

	steered := make(chan error, 1)
	text, _, _, _ = run(agent.TurnRequest{Cwd: dir, Agent: agent.KindAntigravity, SessionID: chat,
		Prompt: "Count from 1 to 80, one number per line, nothing else."}, "t3", func(key string) {
		steered <- p.Steer(ctx, "t3", agent.TurnRequest{ChatID: key, Prompt: "Also: after the last number, write a line that says BANANA."})
	})
	if err := <-steered; err != nil {
		t.Fatalf("steer: %v", err)
	}
	if !strings.Contains(text, "BANANA") {
		t.Fatalf("the steer wasn't followed: %q", text[max(0, len(text)-200):])
	}
	t.Logf("steered turn ends %q", text[max(0, len(text)-40):])
}

// TestLiveAgyUsage reads the installed agy's conversations as the usage
// ledger does and totals the tokens per model. REPOGO_AGY_LIVE=1 runs it.
func TestLiveAgyUsage(t *testing.T) {
	if os.Getenv("REPOGO_AGY_LIVE") != "1" {
		t.Skip("REPOGO_AGY_LIVE=1 runs it against the installed agy")
	}
	p := New(agent.Dependencies{Context: t.Context(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer p.Close()
	type total struct{ calls, uncached, cached, output int64 }
	byModel := map[string]*total{}
	conversations, withCwd := map[string]bool{}, map[string]bool{}
	_ = filepath.WalkDir(p.UsageRoots()[0], func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		parse := p.NewFileUsageParser(path)
		raw, _ := os.ReadFile(path)
		for _, line := range strings.Split(string(raw), "\n") {
			r := parse([]byte(line))
			if r == nil {
				continue
			}
			conversations[r.Session] = true
			if r.Cwd != "" {
				withCwd[r.Session] = true
			}
			m := byModel[r.Model]
			if m == nil {
				m = &total{}
				byModel[r.Model] = m
			}
			m.calls++
			m.uncached += r.Tokens.Uncached
			m.cached += r.Tokens.Cached
			m.output += r.Tokens.Output
		}
		return nil
	})
	if len(byModel) == 0 {
		t.Fatal("no model calls read")
	}
	t.Logf("%d conversations, %d with a folder", len(conversations), len(withCwd))
	for model, m := range byModel {
		t.Logf("%s: %d calls, %d input, %d cached, %d output", model, m.calls, m.uncached, m.cached, m.output)
	}
}

// TestLiveAgyHistory lists the installed agy's own chats as the chat list
// would and reads the newest. REPOGO_AGY_LIVE=1 runs it.
func TestLiveAgyHistory(t *testing.T) {
	if os.Getenv("REPOGO_AGY_LIVE") != "1" {
		t.Skip("REPOGO_AGY_LIVE=1 runs it against the installed agy")
	}
	p := New(agent.Dependencies{Context: t.Context(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer p.Close()
	metas, err := p.Sessions().List()
	if err != nil {
		t.Fatal(err)
	}
	var newest session.Meta
	for _, m := range metas {
		t.Logf("%s %q in %s", m.ID[:8], m.Title, m.Cwd)
		if m.UpdatedAt.After(newest.UpdatedAt) && filepath.Base(m.Path) == usageLog {
			newest = m
		}
	}
	if newest.ID == "" {
		t.Skip("agy has no chats of its own here")
	}
	events, err := session.ReadAll(newest, p.Sessions())
	if err != nil {
		t.Fatal(err)
	}
	counts := map[agent.EventKind]int{}
	unmatched := 0
	for _, e := range events {
		counts[e.Kind]++
		if e.Kind == agent.EventToolResult && strings.HasPrefix(e.Tool.CallID, logResult) {
			unmatched++
		}
	}
	t.Logf("newest %s: %v, %d results unmatched", newest.ID[:8], counts, unmatched)
	// Every tool call in every chat, as its row will read.
	seen := map[string]bool{}
	for _, m := range metas {
		all, _ := session.ReadAll(m, p.Sessions())
		for _, e := range all {
			if e.Kind != agent.EventToolCall {
				continue
			}
			// One row per icon and tool, enough to see each kind.
			row := p.ToolRow(*e.Tool)
			key := row.Icon + " " + toolOf(e.Tool.Name, map[string]any{})
			if !seen[key] {
				seen[key] = true
				t.Log(e.Tool.Name + " → " + row.Icon + " | " + row.Labels.Completed + " | file " + row.File)
			}
		}
	}
}
