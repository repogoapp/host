package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/apphome"
)

// The help lists every command once, one per line, with --flags; a command's
// own help lists its flags, hidden ones included.
func TestHelpListsEveryCommand(t *testing.T) {
	var out bytes.Buffer
	printHelp(&out)
	help := out.String()
	for _, c := range commands {
		if !c.hidden && !strings.Contains(help, strings.TrimSpace(commandName(c.name)+" "+c.args)) {
			t.Errorf("help is missing %q", commandName(c.name))
		}
	}
	if strings.Contains(help, " -lan") || strings.Contains(help, "invite") {
		t.Fatalf("help has a single-dash flag or the removed invite:\n%s", help)
	}
	out.Reset()
	pair, _ := find("pair")
	printCommand(&out, pair)
	if !strings.Contains(out.String(), "--reusable") || !strings.Contains(out.String(), "--pair-host") {
		t.Fatalf("repogo help pair:\n%s", out.String())
	}
}

// A flag belongs to its command: one another command takes is refused, before
// anything runs.
func TestAFlagIsRefusedOnACommandWithoutIt(t *testing.T) {
	for _, args := range [][]string{{"stop", "--now"}, {"status", "--yes"}, {"pair", "--lan"}, {"version", "--all"}} {
		err := dispatch(context.Background(), args)
		if err == nil || !strings.Contains(err.Error(), "unknown flag") || !strings.Contains(err.Error(), "repogo help "+args[0]) {
			t.Errorf("%v: %v", args, err)
		}
	}
	if err := dispatch(context.Background(), []string{"nope"}); err == nil || !strings.Contains(err.Error(), `unknown command "nope"`) {
		t.Fatalf("an unknown command: %v", err)
	}
	if err := dispatch(context.Background(), []string{"version", "extra"}); err == nil {
		t.Fatal("version took an argument")
	}
}

// Flags may follow a command's own words: repogo devices revoke --all.
func TestFlagsMayFollowTheCommandsWords(t *testing.T) {
	fs := flag.NewFlagSet("repogo devices", flag.ContinueOnError)
	all := fs.Bool("all", false, "")
	yes := fs.Bool("yes", false, "")
	words, err := parseAll(fs, []string{"revoke", "--all", "--yes"})
	if err != nil || !*all || !*yes || len(words) != 1 || words[0] != "revoke" {
		t.Fatalf("words %v, all %t, yes %t, err %v", words, *all, *yes, err)
	}
}

func answering(t *testing.T, terminal bool, typed string) {
	t.Helper()
	old := stdin
	stdin.Reader = strings.NewReader(typed)
	stdin.terminal = func() bool { return terminal }
	t.Cleanup(func() { stdin = old })
}

// A step that can't be undone asks first: --yes answers it, a "y" at a
// terminal agrees, anything else leaves things as they were, and with no
// terminal it refuses rather than waiting.
func TestConfirmAsksOrRefuses(t *testing.T) {
	answering(t, false, "")
	if err := confirm("Go?", true); err != nil {
		t.Fatalf("--yes: %v", err)
	}
	if err := confirm("Go?", false); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("no terminal: %v", err)
	}
	answering(t, true, "y\n")
	if err := confirm("Go?", false); err != nil {
		t.Fatalf("y: %v", err)
	}
	answering(t, true, "\n")
	if err := confirm("Go?", false); !errors.Is(err, errDeclined) {
		t.Fatalf("enter: %v", err)
	}
}

// Installing links repogo onto PATH from ~/.local/bin, adds that folder to
// the shell's profile once when PATH lacks it, and never replaces a file it
// didn't make; uninstalling removes only its own link.
func TestRepogoIsLinkedOntoPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(apphome.EnvVar, filepath.Join(home, ".repogo"))
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", "")
	target := filepath.Join(home, ".repogo", "bin", "repogo")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(profile, []byte("alias ll='ls -l'"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	linkOnPath(&out)
	link := filepath.Join(home, ".local", "bin", "repogo")
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("link %q, %v", got, err)
	}
	if !strings.Contains(out.String(), "Added ~/.local/bin to PATH in ~/.zshrc") {
		t.Fatalf("no PATH note: %q", out.String())
	}
	data, _ := os.ReadFile(profile)
	if !strings.HasPrefix(string(data), "alias ll='ls -l'\n\n") || strings.Count(string(data), `export PATH="$HOME/.local/bin:$PATH"`) != 1 {
		t.Fatalf("profile: %q", data)
	}
	// Linked, profile written: nothing to say or add, every run after the
	// first, even before a new terminal picks up PATH.
	out.Reset()
	linkOnPath(&out)
	if again, _ := os.ReadFile(profile); out.Len() != 0 || string(again) != string(data) {
		t.Fatalf("a second run said %q, profile %q", out.String(), again)
	}
	// The folder on PATH already: the profile is never touched.
	os.Remove(link)
	os.Remove(profile)
	t.Setenv("PATH", filepath.Dir(link)+":/usr/bin")
	linkOnPath(&out)
	if _, err := os.Stat(profile); out.Len() != 0 || !os.IsNotExist(err) {
		t.Fatalf("with the folder on PATH: said %q, profile %v", out.String(), err)
	}
	unlinkFromPath()
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("uninstall left the link: %v", err)
	}
	// Someone else's repogo there stays, through install and uninstall.
	if err := os.WriteFile(link, []byte("theirs"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkOnPath(&out)
	unlinkFromPath()
	if data, err := os.ReadFile(link); err != nil || string(data) != "theirs" {
		t.Fatalf("another repogo was replaced: %q %v", data, err)
	}
}

// Each shell gets the profile it reads at startup.
func TestShellProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", "")
	bash := ".bashrc"
	if runtime.GOOS == "darwin" {
		bash = ".bash_profile"
	}
	for shell, want := range map[string]string{
		"/bin/zsh":               ".zshrc",
		"/bin/bash":              bash,
		"/opt/homebrew/bin/fish": filepath.Join(".config", "fish", "conf.d", "repogo.fish"),
		"/bin/sh":                ".profile",
	} {
		t.Setenv("SHELL", shell)
		got, line, ok := shellProfile()
		if !ok || got != filepath.Join(home, want) || line == "" {
			t.Errorf("%s: %q %q %v", shell, got, line, ok)
		}
	}
}
