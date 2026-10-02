package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/testwait"
)

// A turn is timed from its Start drop to its Stop drop; whole seconds there
// made the app read up to a second longer than the CLI's own "Cooked for".
func TestParseDropKeepsMilliseconds(t *testing.T) {
	d := drop{Agent: "claude", Event: "Stop", AtMs: 1790210782641, Payload: json.RawMessage(`{"session_id":"s1","cwd":"/tmp"}`)}
	n, ok := parseDrop(d, []Provider{testHooks{}})
	if !ok {
		t.Fatal("drop rejected")
	}
	if want := time.UnixMilli(1790210782641); !n.At.Equal(want) {
		t.Fatalf("At = %v, want %v", n.At.UnixMilli(), want.UnixMilli())
	}
}

// The script itself, run by sh as the agent runs it: the drop it writes must
// carry a millisecond stamp and be named by it.
func TestHelperWritesMilliseconds(t *testing.T) {
	dir := t.TempDir()
	drops := filepath.Join(dir, "notify")
	script := filepath.Join(dir, "repogo-notify")
	if err := os.WriteFile(script, []byte(helperScript(drops)), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script, "claude", "Stop", `{"session_id":"s1"}`)
	before := time.Now()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	after := time.Now()

	entries, err := os.ReadDir(drops)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want one drop, got %v (%v)", entries, err)
	}
	name := entries[0].Name()
	raw, err := os.ReadFile(filepath.Join(drops, name))
	if err != nil {
		t.Fatal(err)
	}
	var d drop
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("drop is not JSON: %v\n%s", err, raw)
	}
	// A date without %N falls back to whole seconds; bound the stamp either way.
	if d.AtMs < before.Truncate(time.Second).UnixMilli() || d.AtMs > after.UnixMilli() {
		t.Fatalf("at_ms %d outside [%d, %d]", d.AtMs, before.UnixMilli(), after.UnixMilli())
	}
	if !strings.HasPrefix(name, fmt.Sprint(d.AtMs)+"-") {
		t.Fatalf("drop %q is not named by its stamp %d", name, d.AtMs)
	}
}

// A host under REPOGO_HOME reads drops from there, so the installed helper
// must write them there too, even for an agent that never saw the variable.
func TestHelperWritesIntoTheHostsHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "it's home")
	t.Setenv(apphome.EnvVar, home)
	t.Setenv("HOME", t.TempDir())
	script, err := WriteHelper()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script, "claude", "Stop", `{"session_id":"s1"}`)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	dir, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("want one drop in %s, got %v (%v)", dir, entries, err)
	}
}

// The questions a hook asks reach subscribers decoded, empty ones dropped.
func TestParseDropDecodesQuestions(t *testing.T) {
	d := drop{Agent: "claude", Event: "PreToolUse", Payload: json.RawMessage(`{"session_id":"s1",
		"tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":" "},{"question":" Ship it? ","header":"Ship","options":[{"label":"Yes"}]}]}}`)}
	n, ok := parseDrop(d, []Provider{testHooks{}})
	if !ok || !n.AsksQuestion() || n.AsksPermission() {
		t.Fatalf("notice %+v not a question", n)
	}
	if len(n.Questions) != 1 || n.Question() != "Ship it?" || n.Questions[0].ID != "1" || len(n.Questions[0].Options) != 1 {
		t.Fatalf("questions = %+v", n.Questions)
	}
}

// The bus drops a subscriber that falls behind; Follow must come back, or a
// slow relay would silence alerts and Live Activities until the host restarts.
func TestFollowSubscribesAgainAfterBeingDropped(t *testing.T) {
	b, err := NewBridge(t.TempDir(), func(Notice) error { return nil }, func(DisplayFrame) {})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	got := make(chan struct{}, 1)
	go b.Follow(ctx, func(n Notice) {
		switch n.Event {
		case "block":
			<-release
		case "after":
			select {
			case got <- struct{}{}:
			default:
			}
		}
	})

	subscribed := func() bool {
		b.subs.mu.Lock()
		defer b.subs.mu.Unlock()
		return len(b.subs.chs) > 0
	}
	testwait.For(t, "Follow to subscribe", subscribed)
	b.subs.publish(Notice{Event: "block"})
	for range subBuffer + 1 {
		b.subs.publish(Notice{Event: "filler"})
	}
	close(release)

	deadline := time.After(5 * time.Second)
	for {
		b.subs.publish(Notice{Event: "after"})
		select {
		case <-got:
			return
		case <-deadline:
			t.Fatal("no notice reached Follow after it was dropped")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type testHooks struct{}

func (testHooks) Kind() agent.Kind                                   { return "claude" }
func (testHooks) Status() AgentStatus                                { return AgentStatus{} }
func (testHooks) Ensure(string) (AgentStatus, error)                 { return AgentStatus{}, nil }
func (testHooks) Remove(string) error                                { return nil }
func (testHooks) HookStatus(string, string, string) agent.ChatStatus { return agent.ChatCompleted }
func (testHooks) Displays() bool                                     { return true }
