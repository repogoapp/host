package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Run is the one git runner for every package: bounded by commandTimeout, no
// prompts, stderr as the error. Stdout comes back even on failure, because
// `diff --exit-code` reports differences with a non-zero status.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	return runRaw(ctx, dir, commandTimeout, "", args...)
}

// runTimeout is Run for a command that legitimately outlives commandTimeout,
// such as a first push of a large branch.
func runTimeout(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	return runRaw(ctx, dir, timeout, "", args...)
}

// run blanks stdout on failure, the right default for reads.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	return runInput(ctx, dir, "", args...)
}

func resolve(ctx context.Context, dir, ref string) (string, error) {
	out, err := run(ctx, dir, "rev-parse", "--verify", ref)
	return strings.TrimSpace(out), err
}

// isRepo is whether dir is inside a work tree; a plain folder is not an error.
func isRepo(ctx context.Context, dir string) bool {
	_, err := run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

// toplevel is the root of the checkout dir is in.
func toplevel(ctx context.Context, dir string) (string, error) {
	root, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	return strings.TrimSpace(root), err
}

func hasOrigin(ctx context.Context, dir string) bool {
	_, err := run(ctx, dir, "remote", "get-url", "origin")
	return err == nil
}

// runInput is run with stdin, for the commands that read paths from it.
func runInput(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	stdout, err := runRaw(ctx, dir, commandTimeout, stdin, args...)
	if err != nil {
		return "", err
	}
	return stdout, nil
}

// `-C dir` rather than cmd.Dir so git resolves worktrees as the user would.
func runRaw(ctx context.Context, dir string, timeout time.Duration, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// A credential prompt or editor would hang here forever.
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")

	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("git %s: %s", args[0], msg)
	}
	return stdout.String(), nil
}
