// Package handoff moves a chat from the phone to the agent's own CLI on this
// environment: the command that resumes it, and a terminal window running it.
package handoff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/syscmd"
)

var (
	ErrNoResume = errkind.New(errkind.Unavailable, "handoff: this agent has no resume command")
	errMacOnly  = errkind.New(errkind.Unavailable, "handoff: opening a terminal is macOS only")
)

// Command is the shell line that resumes sessionID from cwd. Claude keeps
// sessions per folder, so the `cd` comes first.
func Command(p agent.Provider, sessionID, cwd string) (string, error) {
	if p.ResumeArgs == nil || sessionID == "" {
		return "", ErrNoResume
	}
	words := []string{p.Command()}
	for _, arg := range p.ResumeArgs(sessionID) {
		words = append(words, quoteArg(arg))
	}
	resume := strings.Join(words, " ")
	if cwd == "" {
		return resume, nil
	}
	return "cd " + quote(cwd) + " && " + resume, nil
}

// Open runs line in a new window of the user's terminal app. macOS opens a
// `.command` file with whichever app is set to run them, so iTerm or Ghostty
// users get theirs; the script deletes itself once it starts.
func Open(ctx context.Context, line string) error {
	if runtime.GOOS != "darwin" {
		return errMacOnly
	}
	script, err := os.CreateTemp("", "repogo-handoff-*.command")
	if err != nil {
		return err
	}
	// After the agent quits the window stays a shell in the project.
	body := "#!/bin/sh\nrm -f -- \"$0\"\n" + line + "\nexec \"${SHELL:-/bin/zsh}\" -l\n"
	if _, err := script.WriteString(body); err != nil {
		script.Close()
		return errors.Join(err, os.Remove(script.Name()))
	}
	if err := script.Close(); err != nil {
		return err
	}
	if err := os.Chmod(script.Name(), 0o700); err != nil {
		return err
	}
	if _, err := syscmd.Output(ctx, 10*time.Second, "/usr/bin/open", script.Name()); err != nil {
		return errors.Join(fmt.Errorf("handoff: %w", err), os.Remove(script.Name()))
	}
	return nil
}

// Single quotes keep spaces and `$` literal; a quote inside closes, escapes
// and reopens.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)

// quoteArg leaves a flag or a session id bare so the line reads as typed.
func quoteArg(s string) string {
	if plainArg.MatchString(s) {
		return s
	}
	return quote(s)
}
