package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
)

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A folder is trusted when Claude saved hasTrustDialogAccepted for it or a
// folder above it; a missing or unreadable file trusts nothing.
func TestTrustedFolderReadsClaudesOwnRecord(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, ".claude.json")
	work, other := filepath.Join(root, "work"), filepath.Join(root, "other")
	if trustedFolder(config, work) {
		t.Fatal("trusted with no file")
	}
	writeFile(t, config, `{"projects":{"`+work+`":{"hasTrustDialogAccepted":true},"`+other+`":{"hasTrustDialogAccepted":false}}}`)
	for dir, want := range map[string]bool{work: true, filepath.Join(work, "app", "web"): true, other: false, root: false} {
		if got := trustedFolder(config, dir); got != want {
			t.Errorf("%s trusted = %v, want %v", dir, got, want)
		}
	}
	writeFile(t, config, `{not json`)
	if trustedFolder(config, work) {
		t.Fatal("an unreadable file trusted a folder")
	}
}

// Trusting writes only the folder's flag: every other key, and the folder's
// other fields, stay as Claude wrote them, and so does the file's mode.
func TestMarkTrustedKeepsTheRestOfClaudesFile(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, ".claude.json")
	work := filepath.Join(root, "work")
	writeFile(t, config, `{"oauthAccount":{"emailAddress":"a@b.c"},"numStartups":7,"projects":{"`+work+`":{"allowedTools":["Bash"],"hasTrustDialogAccepted":false},"/elsewhere":{"hasTrustDialogAccepted":true}}}`)
	if err := os.Chmod(config, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := markTrusted(config, work); err != nil {
		t.Fatal(err)
	}
	if !trustedFolder(config, work) || !trustedFolder(config, "/elsewhere") {
		t.Fatal("trust not saved, or another folder's lost")
	}
	data, _ := os.ReadFile(config)
	var got struct {
		OAuth    map[string]string `json:"oauthAccount"`
		Startups int               `json:"numStartups"`
		Projects map[string]struct {
			AllowedTools []string `json:"allowedTools"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.OAuth["emailAddress"] != "a@b.c" || got.Startups != 7 || len(got.Projects[work].AllowedTools) != 1 {
		t.Fatalf("other keys changed: %s", data)
	}
	if info, _ := os.Stat(config); info.Mode().Perm() != 0o644 {
		t.Fatalf("mode changed to %o", info.Mode().Perm())
	}
	// No file yet: one is made holding only the trust.
	fresh := filepath.Join(root, "new", ".claude.json")
	if err := markTrusted(fresh, work); err != nil || !trustedFolder(fresh, work) {
		t.Fatalf("a new file didn't hold the trust: %v", err)
	}
}

// What a repo's settings would do is named for the card: hooks, rules and
// keys that run code; settings that change none of that need no card.
func TestProjectSettingsNameWhatWouldRun(t *testing.T) {
	dir := t.TempDir()
	if got := projectSettings(dir); len(got) != 0 {
		t.Fatalf("no settings: %v", got)
	}
	writeFile(t, filepath.Join(dir, ".claude", "settings.json"), `{"model":"sonnet","permissions":{"deny":["Bash(rm:*)"]}}`)
	if got := projectSettings(dir); len(got) != 0 {
		t.Fatalf("harmless settings named: %v", got)
	}
	writeFile(t, filepath.Join(dir, ".claude", "settings.json"), `{
		"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"./setup.sh"}]}]},
		"permissions":{"allow":["Bash(*)"],"defaultMode":"bypassPermissions"},
		"apiKeyHelper":"./key.sh","env":{"NODE_OPTIONS":"--require ./x.js"},
		"enableAllProjectMcpServers":true}`)
	writeFile(t, filepath.Join(dir, ".claude", "settings.local.json"), `{oops`)
	got := strings.Join(projectSettings(dir), "\n")
	for _, want := range []string{"SessionStart: ./setup.sh", "Bash(*)", "bypassPermissions", "apiKeyHelper: ./key.sh",
		"NODE_OPTIONS", "every MCP server", "settings.local.json can't be read"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// trustRun is a turn in a repo with a hook, through the real runner and a
// fake claude that records its arguments.
type trustRun struct {
	r      *runner
	cwd    string
	config string
	args   string
	asked  int
}

func newTrustRun(t *testing.T, hook bool) *trustRun {
	r := directTestRunner(t)
	run := &trustRun{r: r, cwd: t.TempDir(), args: filepath.Join(t.TempDir(), "args")}
	run.config = r.configFile()
	r.deps.Env = append(r.deps.Env, "REPOGO_FAKE_CLAUDE_ARGS="+run.args)
	if hook {
		writeFile(t, filepath.Join(run.cwd, ".claude", "settings.json"),
			`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"./setup.sh"}]}]}}`)
	}
	return run
}

// send runs one turn in a new chat, answering the trust card with pick ("" errs).
func (run *trustRun) send(t *testing.T, chat, pick string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	io := agent.TurnIO{TurnID: chat, Emit: func(agent.Event) {}, Ask: func(_ context.Context, a agent.Approval) (json.RawMessage, error) {
		run.asked++
		if a.Kind != agent.ApprovalQuestion || !strings.Contains(a.Questions[0].Text, "./setup.sh") {
			t.Errorf("the card doesn't name the hook: %+v", a)
		}
		if pick == "" {
			return nil, errors.New("dismissed")
		}
		return json.Marshal(map[string]string{trustQuestionID: pick})
	}}
	if _, err := run.r.Send(ctx, agent.TurnRequest{ChatID: chat, Cwd: run.cwd, Prompt: "hello"}, io); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(run.args)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, arg := range strings.Fields(lines[len(lines)-1]) {
		if sources, ok := strings.CutPrefix(arg, "--setting-sources="); ok {
			return sources
		}
	}
	return ""
}

// An untrusted repo with a hook asks once: Not now runs with the user's own
// settings, and later chats in it aren't asked again until a restart.
func TestAnUntrustedRepoRunsWithTheUsersSettingsOnly(t *testing.T) {
	run := newTrustRun(t, true)
	if got := run.send(t, "new-1", trustNo); got != "user" {
		t.Fatalf("sources %q, want user", got)
	}
	if got := run.send(t, "new-2", trustYes); got != "user" || run.asked != 1 {
		t.Fatalf("asked %d times, sources %q", run.asked, got)
	}
	if trustedFolder(run.config, run.cwd) {
		t.Fatal("Not now saved trust")
	}
}

// Trust loads the repo's settings and saves Claude's own flag, so neither
// RepoGo nor a terminal asks again.
func TestTrustingARepoSavesClaudesFlag(t *testing.T) {
	run := newTrustRun(t, true)
	if got := run.send(t, "new-1", trustYes); got != "user,project,local" {
		t.Fatalf("sources %q", got)
	}
	if !trustedFolder(run.config, run.cwd) {
		t.Fatal("trust wasn't saved to Claude's file")
	}
	if run.send(t, "new-2", ""); run.asked != 1 {
		t.Fatalf("asked again after trusting: %d", run.asked)
	}
}

// A folder the terminal trusts, or one with no settings that run anything,
// loads everything as before, with no card; a dismissed card is a no.
func TestTrustedOrPlainReposAreNotAsked(t *testing.T) {
	trusted := newTrustRun(t, true)
	if err := markTrusted(trusted.config, trusted.cwd); err != nil {
		t.Fatal(err)
	}
	if got := trusted.send(t, "new-1", ""); got != "user,project,local" || trusted.asked != 0 {
		t.Fatalf("trusted: sources %q, asked %d", got, trusted.asked)
	}
	plain := newTrustRun(t, false)
	if got := plain.send(t, "new-1", ""); got != "user,project,local" || plain.asked != 0 {
		t.Fatalf("plain: sources %q, asked %d", got, plain.asked)
	}
	dismissed := newTrustRun(t, true)
	if got := dismissed.send(t, "new-1", ""); got != "user" {
		t.Fatalf("dismissed: sources %q", got)
	}
}

// A chat in the home folder finds Claude's own ~/.claude, whose hooks are the
// user's settings, loaded on every turn: nothing to trust, nothing to ask.
func TestTheHomeFoldersOwnClaudeSettingsAreNotAsked(t *testing.T) {
	home := newTrustRun(t, true)
	home.r.home = filepath.Join(home.cwd, ".claude")
	if got := home.send(t, "new-1", ""); got != "user,project,local" || home.asked != 0 {
		t.Fatalf("home: sources %q, asked %d", got, home.asked)
	}
}
