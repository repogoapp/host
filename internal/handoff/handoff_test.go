package handoff

import (
	"errors"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/clitool"
)

func provider(command string, resume func(string) []string) agent.Provider {
	return agent.Provider{Tool: clitool.Tool{Spec: clitool.Spec{Command: command}}, ResumeArgs: resume}
}

func TestCommandResumesFromTheChatsFolder(t *testing.T) {
	claude := provider("claude", func(id string) []string { return []string{"--resume", id} })
	codex := provider("codex", func(id string) []string { return []string{"resume", id} })
	for _, c := range []struct {
		p       agent.Provider
		id, cwd string
		want    string
	}{
		{claude, "abc-123", "/Users/me/app", "cd '/Users/me/app' && claude --resume abc-123"},
		{codex, "019a", "/Users/me/app", "cd '/Users/me/app' && codex resume 019a"},
		{claude, "s", "/tmp/it's here", `cd '/tmp/it'\''s here' && claude --resume s`},
		{codex, "s", "", "codex resume s"},
		{claude, "a b", "", "claude --resume 'a b'"},
	} {
		got, err := Command(c.p, c.id, c.cwd)
		if err != nil || got != c.want {
			t.Errorf("Command(%q, %q) = %q, %v; want %q", c.id, c.cwd, got, err, c.want)
		}
	}
}

func TestCommandNeedsAResumeAndASession(t *testing.T) {
	if _, err := Command(provider("cursor", nil), "s", "/a"); !errors.Is(err, ErrNoResume) {
		t.Errorf("an agent without resume: %v", err)
	}
	claude := provider("claude", func(id string) []string { return []string{"--resume", id} })
	if _, err := Command(claude, "", "/a"); !errors.Is(err, ErrNoResume) {
		t.Errorf("an empty session: %v", err)
	}
}
