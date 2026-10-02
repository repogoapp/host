package power

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
)

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
	tmp, err := os.CreateTemp("", "repogo-sudoers-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(rule); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// A broken file in sudoers.d breaks sudo itself, so it is checked before it lands.
	if err := sudo(ctx, "/usr/sbin/visudo", "-cf", tmp.Name()); err != nil {
		return fmt.Errorf("sudoers rule failed validation: %w", err)
	}
	return sudo(ctx, "/usr/bin/install", "-m", "0440", "-o", "root", "-g", "wheel", tmp.Name(), SudoersPath)
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

// Status describes the lid hold for `repogo power status`.
func Status(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errMacOnly
	}
	rule := "off (run repogo power enable)"
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
