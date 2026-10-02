package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// oneShotTimeout bounds a throwaway CLI read; its callers cache the answer and
// have a fallback, so a slow CLI costs a stale answer, not a hung request.
const oneShotTimeout = 20 * time.Second

// oneShot is a throwaway CLI process with the same environment a turn gets, so
// a host launched from inside Claude Code does not pass on its session.
func oneShot(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = ChildEnv()
	// A neutral directory: the answer is the CLI's, not a project's.
	cmd.Dir = os.TempDir()
	return cmd
}

// ChildEnv is this process's environment minus what a Claude Code session
// stamps on its descendants: a claude that thinks it is a child session runs
// the turn but never writes its transcript, and the chat goes silent.
func ChildEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == "CLAUDECODE" || key == "CLAUDE_PID" || key == "CLAUDE_EFFORT" ||
			key == "CLAUDE_AGENT_SDK_VERSION" || strings.HasPrefix(key, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Exchange writes lines to a throwaway CLI and reads replies until want of them
// are matched, then kills it; match names the reply a stdout line carries.
func Exchange(ctx context.Context, name string, args, lines []string, want int,
	match func([]byte) (string, json.RawMessage, bool)) (map[string]json.RawMessage, bool) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, oneShotTimeout)
	defer cancel()

	cmd := oneShot(ctx, name, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, false
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false
	}
	if cmd.Start() != nil {
		return nil, false
	}
	defer func() {
		stdin.Close()
		cancel()
		_ = cmd.Wait()
	}()

	for _, line := range lines {
		if _, err := io.WriteString(stdin, line+"\n"); err != nil {
			return nil, false
		}
	}

	out := map[string]json.RawMessage{}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for len(out) < want && scanner.Scan() {
		if key, result, ok := match(scanner.Bytes()); ok {
			out[key] = result
		}
	}
	return out, len(out) == want
}
