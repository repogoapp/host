//go:build unix

package clilogin

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A CLI started through a launcher (wrangler's) leaves its child running
// unless the whole group is stopped; a cancelled sign-in stops both.
func TestCancelStopsTheCLIsChildren(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "cli")
	script := "#!/bin/sh\nsh -c 'echo $$ > \"$DIR/child\"; while true; do sleep 0.05; done' &\n" +
		"while [ ! -s \"$DIR/child\" ]; do sleep 0.01; done\necho 'https://example.com/authorize'\nwait\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ended := make(chan bool, 1)
	l := Logins{Log: quiet(), Grace: time.Millisecond}
	_, err := l.Start("cli", Spec{
		Command: cli, Env: []string{"DIR=" + dir}, Timeout: time.Minute,
		Read:  func(line string, c *Code) bool { c.URL = line; return true },
		OnEnd: func(ok bool) { ended <- ok },
	})
	if err != nil {
		t.Fatal(err)
	}
	l.Cancel("cli")
	waitEnd(t, ended)
	pid, _ := os.ReadFile(filepath.Join(dir, "child"))
	child, err := strconv.Atoi(strings.TrimSpace(string(pid)))
	if err != nil {
		t.Fatalf("child pid %q: %v", pid, err)
	}
	// Signal 0 only asks whether the process exists; the kill is synchronous.
	if syscall.Kill(child, 0) == nil {
		t.Fatalf("the CLI's child %d outlived the sign-in", child)
	}
}
