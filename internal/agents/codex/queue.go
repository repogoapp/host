package codex

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/agent"
)

const queueTimeout = 20 * time.Second

// queue hands a turn to the Codex app that holds the chat (the desktop app
// runs a server of its own), through Codex's message queue, which that app
// reads: `codex queue`. The turn ends once queued; the prompt and the answer
// reach the phone from the rollout, as a turn run on the environment does.
func (r *runner) queue(ctx context.Context, req agent.TurnRequest) (agent.Result, error) {
	if len(req.Attachments) > 0 || strings.TrimSpace(req.Prompt) == "" {
		return agent.Result{}, errOpenElsewhere
	}
	ctx, cancel := context.WithTimeout(ctx, queueTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.executable(), "queue", "--thread", req.SessionID, "--message", req.Prompt)
	cmd.Dir, cmd.Env = os.TempDir(), append(r.environment(), "CODEX_HOME="+r.home)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.deps.Log.Warn("Codex queue failed", "chat", req.ChatID, "error", err, "output", lastLine(string(out)))
		return agent.Result{}, errOpenElsewhere
	}
	return agent.Result{SessionID: req.SessionID, StopReason: "queued"}, nil
}
