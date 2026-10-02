// Package syscmd runs a short system tool (pmset, launchctl, systemctl) and
// returns its output, with that output in the error when the tool fails.
package syscmd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Output runs name with args for at most timeout and returns its combined output.
func Output(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
