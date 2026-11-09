package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/errkind"
)

// OnceAnswerer is an Adapter that answers one prompt in a throwaway run: no session
// saved, no hooks, no tools, so nothing it does shows up as a chat.
type OnceAnswerer interface {
	AnswerOnce(ctx context.Context, model, prompt string) (string, error)
}

// onceTimeout bounds a throwaway answer; a CLI that hangs costs nothing but it.
const onceTimeout = 45 * time.Second

// AnswerOnce answers prompt with kind's agent on model ("" for its default), off any
// chat. An agent without a throwaway mode is refused.
func (m *Manager) AnswerOnce(ctx context.Context, kind Kind, model, prompt string) (string, error) {
	answerer, ok := m.adapters[kind].(OnceAnswerer)
	if !ok {
		return "", fmt.Errorf("%w: %s can't answer off a chat", errkind.ErrUnavailable, kind)
	}
	ctx, cancel := context.WithTimeout(ctx, onceTimeout)
	defer cancel()
	return answerer.AnswerOnce(ctx, model, prompt)
}

// RunOnce runs a throwaway CLI in a neutral folder with prompt on stdin, and
// returns what it printed. env is the whole environment; ChildEnv is its base.
func RunOnce(ctx context.Context, name string, env []string, prompt string, args ...string) (string, error) {
	cmd := oneShot(ctx, name, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if _, isExit := err.(*exec.ExitError); isExit {
			return "", fmt.Errorf("%s: %w: %s", name, err, lastLine(stderr.String()))
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
