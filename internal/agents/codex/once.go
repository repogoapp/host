package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/repogoapp/host/internal/agent"
)

// AnswerOnce answers prompt in a `codex exec` that saves no session, runs no
// hooks or MCP servers and can't write, so it never shows up as a chat. Low
// effort: an answer off a chat is a small one.
func (r *runner) AnswerOnce(ctx context.Context, model, prompt string) (string, error) {
	dir, err := os.MkdirTemp("", "repogo-once-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	answer := filepath.Join(dir, "answer.txt")
	args := []string{"exec", "--ephemeral", "--skip-git-repo-check", "-s", "read-only",
		"-c", "features.hooks=false", "-c", "mcp_servers={}", "-c", `model_reasoning_effort="low"`,
		"-o", answer}
	if model != "" {
		args = append(args, "-m", model)
	}
	// "-" reads the prompt from stdin, so one that starts with a dash stays a prompt.
	args = append(args, "-")
	env := agent.ChildEnv()
	if r.deps.Root != "" {
		env = append(env, "CODEX_HOME="+r.home)
	}
	if _, err := agent.RunOnce(ctx, r.executable(), env, prompt, args...); err != nil {
		return "", err
	}
	out, err := os.ReadFile(answer)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
