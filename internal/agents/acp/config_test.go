package acp

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
	"github.com/repogoapp/host/internal/projectagent"
)

// shipped is a runner for a shipped agent, the fake one, configured by cfg.
func shipped(t *testing.T, cfg Config, env ...string) (*Provider, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.Kind, cfg.Name, cfg.Home = "shipped", "Shipped", "shipped"
	cfg.Find = func(cwd, _ string) (projectagent.Manifest, error) {
		return projectagent.Manifest{ID: "shipped", Name: "Shipped", Command: executable,
			Dir: filepath.Dir(executable), Project: cwd, Fingerprint: executable}, nil
	}
	p := NewWith(agent.Dependencies{Root: t.TempDir(), Context: t.Context(), Env: append([]string{"REPOGO_FAKE_ACP=1"}, env...),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, cfg)
	t.Cleanup(p.Close)
	return p, dir
}

// A turn's config reaches the session as the options its Settings names.
func TestSettingsSetTheSessionsOptions(t *testing.T) {
	p, dir := shipped(t, Config{Settings: func(c agent.TurnConfig) map[string][]string {
		return map[string][]string{"mode": {c.Mode, "default"}, "missing": {"x"}}
	}}, "REPOGO_FAKE_ACP_MODES=1")
	first := send(t, p, agent.TurnRequest{Cwd: dir, Prompt: "mode", Config: agent.TurnConfig{Mode: "plan"}}, allowAll)
	if first.err != nil || first.text != "plan" {
		t.Fatalf("plan: %v %q", first.err, first.text)
	}
	// A value the agent doesn't offer falls back to the next one wanted.
	odd := send(t, p, agent.TurnRequest{Cwd: dir, SessionID: first.chat, Prompt: "mode", Config: agent.TurnConfig{Mode: "yolo"}}, allowAll)
	if odd.err != nil || odd.text != "default" {
		t.Fatalf("unoffered: %v %q", odd.err, odd.text)
	}
}

// A tool the Config allows runs without asking the phone.
func TestAllowedToolsRunUnasked(t *testing.T) {
	p, dir := shipped(t, Config{Allows: func(agent.TurnConfig, string) bool { return true }})
	got := send(t, p, agent.TurnRequest{Cwd: dir, Prompt: "tool"}, allowAll)
	if got.err != nil || len(got.asked) != 0 || got.text != "rolled" {
		t.Fatalf("err %v asked %+v text %q", got.err, got.asked, got.text)
	}
}

// A message sent during a turn joins it: the turn lasts until the steered
// prompt's answer, and its history has the message where it was sent.
func TestSteerJoinsTheRunningTurn(t *testing.T) {
	p, dir := shipped(t, Config{Steers: true})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var mu sync.Mutex
	var text strings.Builder
	var chat string
	started := make(chan struct{})
	var once sync.Once
	io := agent.TurnIO{TurnID: "turn-steer",
		Emit: func(e agent.Event) {
			mu.Lock()
			defer mu.Unlock()
			if e.Kind == agent.EventText {
				text.WriteString(e.Text)
				once.Do(func() { close(started) })
			}
		},
		Session: func(id, _ string) { mu.Lock(); chat = id; mu.Unlock() },
		Ask:     func(context.Context, agent.Approval) (json.RawMessage, error) { return nil, nil },
	}
	type outcome struct {
		result agent.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := p.Send(ctx, agent.TurnRequest{Cwd: dir, Prompt: "steer"}, io)
		done <- outcome{r, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("the turn never started")
	}
	mu.Lock()
	key := agent.ChatID("shipped", chat)
	mu.Unlock()
	if err := p.Steer(ctx, "another-turn", agent.TurnRequest{ChatID: key, Prompt: "x"}); err == nil {
		t.Fatal("steered a turn that isn't running")
	}
	if err := p.Steer(ctx, "turn-steer", agent.TurnRequest{ChatID: key, Prompt: "this"}); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.result.StopReason != stopEndTurn || text.String() != "counting then this" {
		t.Fatalf("err %v stop %q text %q", got.err, got.result.StopReason, text.String())
	}
	history := strings.Join(kinds(history(t, p, chat)), " | ")
	if history != "turn_started: | user_message:steer | text:counting | user_message:this | text: then this | turn_finished:" {
		t.Fatalf("history %s", history)
	}
	// Once the turn is over a steer is refused, so it is sent as a turn.
	if err := p.Steer(ctx, "turn-steer", agent.TurnRequest{ChatID: key, Prompt: "late"}); err == nil {
		t.Fatal("steered a finished turn")
	}
}

// Without Steers a message during a turn is refused, and queues instead.
func TestAgentsThatDontSteerRefuse(t *testing.T) {
	p, _ := shipped(t, Config{})
	if err := p.Steer(t.Context(), "t", agent.TurnRequest{ChatID: "c", Prompt: "x"}); err == nil {
		t.Fatal("steer accepted")
	}
}

// An edit whose title climbs out of the folder is named by its file.
func TestEditsAreNamedByTheirFile(t *testing.T) {
	st := toolState{title: "File ../../../../tmp/work/hi.txt", kind: "edit", path: "/tmp/work/hi.txt"}
	if got := st.label("/tmp/work"); got != "Edit hi.txt" {
		t.Fatalf("label %q", got)
	}
	st = toolState{title: "Write notes", kind: "edit", path: "/tmp/work/hi.txt"}
	if got := st.label("/tmp/work"); got != "Write notes" {
		t.Fatalf("label %q", got)
	}
}

// The agent's name for a chat titles it once; a rename stays the user's.
func TestTheAgentsTitleNamesTheChatOnce(t *testing.T) {
	dir, root := project(t)
	p := provider(t, root)
	first := send(t, p, agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "hello"}, allowAll)
	metas, _ := p.Sessions().List()
	if first.err != nil || len(metas) != 1 || metas[0].Title != "Greeting" {
		t.Fatalf("err %v metas %+v", first.err, metas)
	}
	if err := p.sessions.Rename(metas[0], "Mine"); err != nil {
		t.Fatal(err)
	}
	send(t, p, agent.TurnRequest{Cwd: dir, SessionID: first.chat, Prompt: "hello"}, allowAll)
	if metas, _ = p.Sessions().List(); metas[0].Title != "Mine" {
		t.Fatalf("title %q", metas[0].Title)
	}
}

// A turn sent to a session the agent ran on its own adopts it under the
// agent's id and reopens it; links name it as its own chat.
func TestAdoptedSessionsAreReopened(t *testing.T) {
	p, dir := shipped(t, Config{}, "REPOGO_FAKE_ACP_LOAD=1")
	// The folder is the test's, known once the runner exists.
	p.cfg.Adopt = func(session string) (string, bool) { return dir, session == "outside" }
	got := send(t, p, agent.TurnRequest{Cwd: dir, SessionID: "outside", Prompt: "hello"}, allowAll)
	if got.err != nil || got.chat != "outside" || got.text != "Hello" {
		t.Fatalf("err %v chat %q text %q", got.err, got.chat, got.text)
	}
	if l := p.Chats().Links()["outside"]; l.Chat != "outside" {
		t.Fatalf("links %+v", p.Chats().Links())
	}
	if bad := send(t, p, agent.TurnRequest{Cwd: dir, SessionID: "unknown", Prompt: "hello"}, allowAll); bad.err == nil {
		t.Fatal("an unknown chat was adopted")
	}
}
