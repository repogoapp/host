package clitool

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Each Fly account is a folder: listed with whoever its folder is signed in
// as, used by agents through FLY_CONFIG_DIR, renamed and removed with it.
func TestFlyAccountsAreFolders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A fly that answers whoami from the folder FLY_CONFIG_DIR names.
	bin := filepath.Join(t.TempDir(), "fly")
	// With no folder named, it answers as the environment's own login.
	script := "#!/bin/sh\n[ \"$1 $2\" = \"auth whoami\" ] || exit 0\n" +
		"[ -n \"$FLY_CONFIG_DIR\" ] || { cat \"$HOME/own-who\"; exit; }\n" +
		"cat \"$FLY_CONFIG_DIR/who\" 2>/dev/null || exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f := flyAccounts{}
	for _, id := range []string{"boppr", "repogo"} {
		if err := f.Prepare(ctx, bin, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(flyDir("boppr"), "who"), []byte("jerrick+boppr@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := f.List(ctx, bin)
	if len(got) != 2 || got[0].ID != "boppr" || !got[0].SignedIn || got[0].Detail != "jerrick+boppr@example.com" ||
		got[1].SignedIn || got[1].Detail != "Signed out" || !got[1].CanSignIn {
		t.Fatalf("accounts = %+v", got)
	}
	if err := os.WriteFile(filepath.Join(home, "own-who"), []byte("jerrick+boppr@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if def := f.Default(ctx, bin, got); def != "boppr" {
		t.Fatalf("default = %q, want the account matching the environment's own login", def)
	}
	if env := f.Env("boppr"); env != "FLY_CONFIG_DIR="+filepath.Join(home, ".repogo", "cli-accounts", "fly", "boppr") {
		t.Fatalf("env = %s", env)
	}
	if !slices.Equal(f.LoginEnv("repogo"), []string{"FLY_CONFIG_DIR=" + flyDir("repogo")}) {
		t.Fatalf("login env = %v", f.LoginEnv("repogo"))
	}
	if detail, suggestion := f.Who(ctx, bin, "boppr"); detail != "jerrick+boppr@example.com" || suggestion != "jerrick-boppr" {
		t.Fatalf("who = %q, %q", detail, suggestion)
	}

	if err := f.Rename(ctx, bin, "boppr", "boppr-prod"); err != nil {
		t.Fatal(err)
	}
	if err := f.Remove(ctx, bin, "repogo"); err != nil {
		t.Fatal(err)
	}
	got = f.List(ctx, bin)
	if len(got) != 1 || got[0].ID != "boppr-prod" || !got[0].SignedIn {
		t.Fatalf("after rename and remove = %+v", got)
	}
}
