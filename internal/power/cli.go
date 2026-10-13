package power

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
)

// sudoersInstall reads the rule from stdin into a root-owned file, checks it
// with visudo and installs it, all in one sudo: a user-owned temp file between
// the check and the install could be swapped for another rule. $1 is the path.
const sudoersInstall = `set -e; f=$(mktemp /tmp/repogo-sudoers.XXXXXX); trap 'rm -f "$f"' EXIT
cat > "$f"; /usr/sbin/visudo -cf "$f"; /usr/bin/install -m 0440 -o root -g wheel "$f" "$1"`

var errMacOnly = errors.New("the lid hold is macOS only")

// Enabled reports whether the sudoers rule is installed.
func Enabled() bool {
	_, err := os.Stat(SudoersPath)
	return err == nil
}

// Enable installs the sudoers rule, asking for the user's password once.
func Enable(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return errMacOnly
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	rule, err := SudoersRule(u.Username)
	if err != nil {
		return err
	}
	// A broken file in sudoers.d breaks sudo itself, so visudo checks it first.
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", "/bin/sh", "-c", sudoersInstall, "sh", SudoersPath)
	cmd.Stdin = strings.NewReader(rule)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installing the sudoers rule: %w", err)
	}
	return nil
}

// Disable removes the rule, then clears a SleepDisabled the host set.
func Disable(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return errMacOnly
	}
	// Removing the rule first stops a running host from setting the flag again.
	if err := sudo(ctx, "/bin/rm", "-f", SudoersPath); err != nil {
		return err
	}
	marker, err := markerPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(marker); err != nil {
		return nil
	}
	if err := sudo(ctx, pmset, pmsetArgs(false)...); err != nil {
		return err
	}
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Status describes the lid hold for `repogo keep-awake status`.
func Status(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errMacOnly
	}
	rule := "off (run repogo keep-awake enable)"
	if Enabled() {
		rule = "enabled (" + SudoersPath + ")"
	}
	out, err := output(ctx, pmset, "-g")
	if err != nil {
		return "", err
	}
	flag := "0"
	if parseSleepDisabled(out) {
		flag = "1, set outside RepoGo"
		if marker, err := markerPath(); err == nil {
			if _, err := os.Stat(marker); err == nil {
				flag = "1, held by the host"
			}
		}
	}
	return fmt.Sprintf("Lid hold: %s\nSleepDisabled: %s\nIdle sleep is held while the host runs with a paired device.", rule, flag), nil
}

func sudo(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", append([]string{name}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
