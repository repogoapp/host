package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testHelper = "/home/u/.repogo/hooks/repogo-notify"

// userHooks is a hooks.json the user wrote: a key of their own, a group on an
// event we also use, and an event we do not.
const userHooks = `{
  "description": "mine",
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "/usr/local/bin/ding", "timeout": 3}]}],
    "SessionStart": [{"matcher": "startup", "hooks": [{"type": "command", "command": "echo hi"}]}]
  }
}`

func testFile(t *testing.T, contents string) HookFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hooks.json")
	if contents != "" {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return HookFile{
		Path: path, Agent: "codex", Events: []string{"UserPromptSubmit", "Stop"},
		Timeouts: map[string]int{"Stop": 5},
	}
}

type fileShape struct {
	Description string `json:"description"`
	Hooks       map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func readShape(t *testing.T, path string) fileShape {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s fileShape
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHookFileEnsureAppendsBesideUserHooks(t *testing.T) {
	f := testFile(t, userHooks)
	if err := f.Ensure(testHelper); err != nil {
		t.Fatal(err)
	}
	s := readShape(t, f.Path)
	if s.Description != "mine" {
		t.Errorf("description = %q", s.Description)
	}
	if got := s.Hooks["SessionStart"]; len(got) != 1 || got[0].Hooks[0].Command != "echo hi" || got[0].Matcher != "startup" {
		t.Errorf("SessionStart = %+v", got)
	}
	stop := s.Hooks["Stop"]
	if len(stop) != 2 || stop[0].Hooks[0].Command != "/usr/local/bin/ding" {
		t.Fatalf("Stop = %+v", stop)
	}
	ours := stop[1].Hooks[0]
	if ours.Command != f.Command(testHelper, "Stop") || ours.Timeout != 5 || stop[1].Matcher != "" {
		t.Errorf("our Stop group = %+v", stop[1])
	}
	if len(s.Hooks["UserPromptSubmit"]) != 1 {
		t.Errorf("UserPromptSubmit = %+v", s.Hooks["UserPromptSubmit"])
	}
	if !f.Installed(testHelper) {
		t.Error("not reported installed")
	}
}

func TestHookFileEnsureTwiceAddsNothing(t *testing.T) {
	f := testFile(t, "")
	for range 2 {
		if err := f.Ensure(testHelper); err != nil {
			t.Fatal(err)
		}
	}
	for event, groups := range readShape(t, f.Path).Hooks {
		if len(groups) != 1 {
			t.Errorf("%s has %d groups", event, len(groups))
		}
	}
}

func TestHookFileRemoveLeavesUserHooks(t *testing.T) {
	f := testFile(t, userHooks)
	if err := f.Ensure(testHelper); err != nil {
		t.Fatal(err)
	}
	if err := f.Remove(testHelper); err != nil {
		t.Fatal(err)
	}
	s := readShape(t, f.Path)
	if s.Description != "mine" || len(s.Hooks) != 2 {
		t.Fatalf("after remove = %+v", s)
	}
	if got := s.Hooks["Stop"]; len(got) != 1 || got[0].Hooks[0].Command != "/usr/local/bin/ding" {
		t.Errorf("Stop = %+v", got)
	}
	if f.Installed(testHelper) {
		t.Error("still reported installed")
	}
}

// A user who moved our handler into their own group keeps the group and
// their handler; only ours goes.
func TestHookFileRemoveFromSharedGroup(t *testing.T) {
	shared := `{"hooks": {"Stop": [{"matcher": "x", "hooks": [
	  {"type": "command", "command": "/usr/local/bin/ding"},
	  {"type": "command", "command": "\"` + testHelper + `\" codex Stop"}]}]}}`
	f := testFile(t, shared)
	if err := f.Remove(testHelper); err != nil {
		t.Fatal(err)
	}
	stop := readShape(t, f.Path).Hooks["Stop"]
	if len(stop) != 1 || stop[0].Matcher != "x" || len(stop[0].Hooks) != 1 || stop[0].Hooks[0].Command != "/usr/local/bin/ding" {
		t.Errorf("Stop = %+v", stop)
	}
}

func TestHookFileRemoveWithoutOursLeavesFileAlone(t *testing.T) {
	f := testFile(t, userHooks)
	if err := f.Remove(testHelper); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != userHooks {
		t.Errorf("file rewritten:\n%s", raw)
	}
	missing := testFile(t, "")
	if err := missing.Remove(testHelper); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing.Path); !os.IsNotExist(err) {
		t.Error("remove created the file")
	}
}

func TestHookFileRefusesUnparseableFile(t *testing.T) {
	const broken = `{"hooks": {"Stop": [`
	f := testFile(t, broken)
	if err := f.Ensure(testHelper); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("Ensure err = %v", err)
	}
	if err := f.Remove(testHelper); err == nil {
		t.Error("Remove accepted a broken file")
	}
	raw, _ := os.ReadFile(f.Path)
	if string(raw) != broken {
		t.Error("broken file rewritten")
	}
}

func TestParseDropReadsCodexModel(t *testing.T) {
	d := drop{Agent: "claude", Event: "UserPromptSubmit", AtMs: 1, Payload: json.RawMessage(
		`{"session_id": "s1", "turn_id": "t1", "model": "gpt-6-astra", "permission_mode": "default", "prompt": "hi"}`)}
	n, ok := parseDrop(d, []Provider{testHooks{}})
	if !ok || n.Model != "gpt-6-astra" || n.Prompt != "hi" || n.TurnID != "t1" {
		t.Errorf("notice = %+v, ok = %v", n, ok)
	}
}
