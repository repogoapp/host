package claude

import (
	"context"

	"github.com/repogoapp/host/internal/agent"
)

// onceSystemPrompt replaces Claude Code's own, which is a coding agent's and
// thousands of tokens: an answer off a chat needs none of it.
const onceSystemPrompt = "Answer with only what is asked."

// AnswerOnce answers prompt in a `claude -p` that saves no session, runs no
// hooks or MCP servers and has no tools, so it never shows up as a chat.
func (r *runner) AnswerOnce(ctx context.Context, model, prompt string) (string, error) {
	args := []string{"-p", "--no-session-persistence", "--tools", "", "--strict-mcp-config",
		"--settings", `{"disableAllHooks":true}`, "--system-prompt", onceSystemPrompt}
	// "default" is the catalog's alias for leaving the choice to Claude.
	if model != "" && model != "default" {
		args = append(args, "--model", model)
	}
	env := agent.ChildEnv()
	if r.deps.Root != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+r.home)
	}
	return agent.RunOnce(ctx, r.executable(), env, prompt, args...)
}
