package codex

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/codexappserver"
)

// Codex runs one background app server the terminal and desktop app share:
// every client sees a thread's turns live, so a chat on it is open in the
// terminal and on the phone at once, and it holds each thread it opened until
// it stops. Without it (older Codex, or turned off) a chat runs on its own app
// server, and Codex lets only one of them write a thread.
const (
	// How long after a failed start to try again, so a Codex that can't run
	// it costs one attempt per window rather than one per chat.
	backgroundRetry = 10 * time.Minute
	backgroundStart = time.Minute
)

type background struct {
	home       string
	executable func() string
	log        *slog.Logger

	mu      sync.Mutex
	retryAt time.Time
}

// dial joins the background server, starting it first when Codex can. false
// means the chat runs on its own app server.
func (b *background) dial(ctx context.Context, handlers codexappserver.Handlers) (*codexappserver.Client, bool) {
	socket := codexappserver.SocketPath(b.home)
	if client, err := codexappserver.Dial(ctx, socket, handlers); err == nil {
		return client, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// Another chat may have started it while this one waited.
	if client, err := codexappserver.Dial(ctx, socket, handlers); err == nil {
		return client, true
	}
	if time.Now().Before(b.retryAt) {
		return nil, false
	}
	if err := b.start(ctx); err != nil {
		b.retryAt = time.Now().Add(backgroundRetry)
		b.log.Info("Codex background server not started; chats run on their own app server", "reason", err)
		return nil, false
	}
	client, err := codexappserver.Dial(ctx, socket, handlers)
	if err != nil {
		b.retryAt = time.Now().Add(backgroundRetry)
		b.log.Warn("Codex background server started but did not answer", "error", err)
		return nil, false
	}
	b.log.Info("Codex background server started")
	return client, true
}

// running reports a background server a new chat would join: its socket is
// there and it was not just found dead.
func (b *background) running() bool {
	b.mu.Lock()
	failed := time.Now().Before(b.retryAt)
	b.mu.Unlock()
	if failed {
		return false
	}
	_, err := os.Stat(codexappserver.SocketPath(b.home))
	return err == nil
}

// start runs `codex app-server daemon start`, which returns once the server
// is up, detached from the host so it outlives a restart as it outlives a
// terminal. It runs with the host's environment, as each chat's own app
// server does, minus REPOGO_ORIGIN: the terminal's turns run there too.
func (b *background) start(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, backgroundStart)
	defer cancel()
	on, err := b.autoStart(ctx)
	if err != nil {
		return err
	}
	if !on {
		return errors.New("Codex's daemon_auto_start feature is off")
	}
	cmd := exec.CommandContext(ctx, b.executable(), "app-server", "daemon", "start")
	cmd.Dir, cmd.Env = os.TempDir(), b.env()
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("codex app-server daemon start: %w (%s)", err, lastLine(string(out)))
	}
	return nil
}

// autoStart is whether Codex starts its background server for the terminal,
// which the host follows: Codex before 0.171 has no such feature, and a user
// who turned it off keeps chats on their own servers.
func (b *background) autoStart(ctx context.Context) (bool, error) {
	cmd := exec.CommandContext(ctx, b.executable(), "features", "list")
	cmd.Dir, cmd.Env = os.TempDir(), b.env()
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("codex features list: %w", err)
	}
	return featureOn(out, "daemon_auto_start"), nil
}

// env names the home the server runs from, the one its socket is under.
func (b *background) env() []string { return append(agent.ChildEnv(), "CODEX_HOME="+b.home) }

// featureOn reads one feature from `codex features list`: its name, stage
// and whether it is on, in columns.
func featureOn(list []byte, name string) bool {
	lines := bufio.NewScanner(bytes.NewReader(list))
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) >= 2 && fields[0] == name {
			return fields[len(fields)-1] == "true"
		}
	}
	return false
}

// shellEnv is the project's variables as Codex's shell_environment_policy:
// a chat on the shared server can't have its own process environment, so
// its commands get them through the thread's config instead.
func shellEnv(env []string) map[string]any {
	set := map[string]any{}
	for _, kv := range env {
		if key, value, ok := strings.Cut(kv, "="); ok && key != "" {
			set[key] = value
		}
	}
	if len(set) == 0 {
		return nil
	}
	return map[string]any{"set": set}
}
