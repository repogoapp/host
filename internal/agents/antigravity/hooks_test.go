package antigravity

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/notify"
)

func TestHooksAreAddedBesideTheUsersOwn(t *testing.T) {
	h := Hooks{Dir: t.TempDir(), Home: t.TempDir()}
	theirs := `{"lint-checker":{"PostToolUse":[{"matcher":"run_command","hooks":[{"type":"command","command":"./lint.sh"}]}]}}`
	if err := os.WriteFile(h.path(), []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}
	helper := "/home/u/.repogo/hooks/repogo-notify"
	if st := h.Status(); st.Installed || st.Conflict != "" {
		t.Fatalf("before %+v", st)
	}
	st, err := h.Ensure(helper)
	if err != nil || !st.Installed {
		t.Fatalf("ensure %+v %v", st, err)
	}
	var all map[string]map[string][]map[string]any
	raw, _ := os.ReadFile(h.path())
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	if _, ok := all["lint-checker"]; !ok || len(all[hookName]) != len(hookEvents) {
		t.Fatalf("file %s", raw)
	}
	// Tool events take a matcher group; the rest a plain handler.
	if all[hookName]["PreToolUse"][0]["matcher"] != "*" || all[hookName]["Stop"][0]["type"] != "command" {
		t.Fatalf("shapes %s", raw)
	}
	if err := h.Remove(helper); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(h.path()); strings.Contains(string(raw), "repogo-notify") || !strings.Contains(string(raw), "lint-checker") {
		t.Fatalf("after remove %s", raw)
	}
}

func TestAHookNamedRepogoThatIsntOursIsLeftAlone(t *testing.T) {
	h := Hooks{Dir: t.TempDir(), Home: t.TempDir()}
	if err := os.WriteFile(h.path(), []byte(`{"repogo":{"Stop":[{"type":"command","command":"./mine.sh"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := h.Ensure("/x/repogo-notify")
	if err != nil || st.Installed || st.Conflict == "" {
		t.Fatalf("%+v %v", st, err)
	}
	if err := os.WriteFile(h.path(), []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Ensure("/x/repogo-notify"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(h.path()); string(raw) != `{not json` {
		t.Fatal("a broken hooks.json was overwritten")
	}
}

// Without agy there is no hooks file to make; removing RepoGo's entry from a
// file that held only it takes the file away too.
func TestHooksLeaveNoTrace(t *testing.T) {
	missing := Hooks{Dir: filepath.Join(t.TempDir(), "config"), Home: filepath.Join(t.TempDir(), "agy")}
	if st, err := missing.Ensure("/x/repogo-notify"); err != nil || st.Installed {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := os.Stat(missing.Dir); !os.IsNotExist(err) {
		t.Fatal("a hooks folder was made without agy")
	}
	h := Hooks{Dir: t.TempDir(), Home: t.TempDir()}
	if st, err := h.Ensure("/x/repogo-notify"); err != nil || !st.Installed {
		t.Fatalf("%+v %v", st, err)
	}
	if err := h.Remove("/x/repogo-notify"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.path()); !os.IsNotExist(err) {
		t.Fatal("an empty hooks.json was left")
	}
	if err := h.Remove("/x/repogo-notify"); err != nil {
		t.Fatal("removing twice failed:", err)
	}
}

// The hook runs the helper for a terminal's chat, not for one RepoGo runs,
// and prints nothing either way: agy reads any output as a decision.
func TestTheHookSkipsRepoGosOwnChats(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "repogo-notify")
	ran := filepath.Join(dir, "ran")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho \"$1 $2\" >> '"+ran+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(origin string) string {
		cmd := exec.Command("sh", "-c", command(helper, "Stop"))
		cmd.Env = append(os.Environ(), notify.OriginEnv+"="+origin)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if out := run(string(notify.OriginApp)); out != "" {
		t.Fatalf("printed %q", out)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the helper ran for RepoGo's own chat")
	}
	if out := run("cli"); out != "" {
		t.Fatalf("printed %q", out)
	}
	if raw, _ := os.ReadFile(ran); string(raw) != "antigravity Stop\n" {
		t.Fatalf("ran %q", raw)
	}
}

func TestHookEventsBecomeStatuses(t *testing.T) {
	h := Hooks{}
	for _, c := range []struct {
		event, reason, tool string
		want                agent.ChatStatus
	}{
		{"PreInvocation", "", "", agent.ChatWorking},
		{"PreToolUse", "", "run_command", agent.ChatWorking},
		{"PreToolUse", "", "ask_question", agent.ChatAwaitingUser},
		{"Stop", "NO_TOOL_CALL", "", agent.ChatCompleted},
		{"Stop", "ERROR", "", agent.ChatFailed},
		{"Stop", "USER_CANCELED", "", agent.ChatInterrupted},
		{"PostInvocation", "", "", agent.ChatUnknown},
	} {
		if got := h.HookStatus(c.event, c.reason, c.tool); got != c.want {
			t.Errorf("%s %s %s: %v", c.event, c.reason, c.tool, got)
		}
	}
}
