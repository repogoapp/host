package clitool

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/errkind"
)

// fakeProfiles is a CLI's accounts, counting how often they are listed.
type fakeProfiles struct{ lists *int }

func (f fakeProfiles) List(context.Context, string) []CLIAccount {
	*f.lists++
	return []CLIAccount{{ID: "client-a", SignedIn: true}, {ID: "client-b"}}
}
func (fakeProfiles) LoginArgs(id string) []string                         { return []string{"login", id} }
func (fakeProfiles) Prepare(context.Context, string, string) error        { return nil }
func (fakeProfiles) LoginEnv(string) []string                             { return nil }
func (fakeProfiles) Default(context.Context, string, []CLIAccount) string { return "client-a" }
func (fakeProfiles) Env(id string) string                                 { return "FAKE_PROFILE=" + id }
func (fakeProfiles) Remove(context.Context, string, string) error         { return nil }
func (fakeProfiles) Rename(context.Context, string, string, string) error { return nil }
func (fakeProfiles) Who(context.Context, string, string) (string, string) { return "", "" }

// A project picks one account; the listing marks it, agents there get its
// variable, the pick survives a reopen, and the listing is cached.
func TestAProjectPicksOneAccount(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakecli")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	lists := 0
	open := func() *Inventory {
		in := NewInventory(nil, nil)
		in.Tools = []Tool{{Kind: "fake", Name: "Fake", Spec: Spec{Command: "fakecli", Accounts: fakeProfiles{&lists}}}}
		if err := in.OpenAccounts(filepath.Join(dir, "cli-accounts.json")); err != nil {
			t.Fatal(err)
		}
		return in
	}
	in := open()
	ctx := context.Background()
	// An id becomes a profile or a folder: only one the CLI has is picked.
	for _, bad := range []string{"../x", "nobody"} {
		if err := in.SelectAccount(ctx, "fake", "/work/app", bad); errkind.Of(err) != errkind.Invalid {
			t.Fatalf("picked %q: %v", bad, err)
		}
	}
	if err := in.SelectAccount(ctx, "fake", "/work/app", "client-b"); err != nil {
		t.Fatal(err)
	}
	got, err := in.Accounts(ctx, "fake", "/work/app")
	if err != nil || len(got.Accounts) != 2 || got.Accounts[0].On || !got.Accounts[1].On ||
		!got.Accounts[0].Default || got.Accounts[1].Default {
		t.Fatalf("accounts = %+v, %v", got, err)
	}
	if env := in.AccountEnv("/work/app"); !slices.Equal(env, []string{"FAKE_PROFILE=client-b"}) {
		t.Fatalf("env = %v", env)
	}
	if env := in.AccountEnv("/work/other"); len(env) != 0 {
		t.Fatalf("another project's env = %v", env)
	}
	in.Accounts(ctx, "fake", "/work/other")
	if lists != 1 {
		t.Fatalf("listed %d times within the TTL", lists)
	}

	reopened := open()
	if env := reopened.AccountEnv("/work/app"); !slices.Equal(env, []string{"FAKE_PROFILE=client-b"}) {
		t.Fatalf("after reopen = %v", env)
	}
	if err := reopened.SelectAccount(ctx, "fake", "/work/app", ""); err != nil || len(reopened.AccountEnv("/work/app")) != 0 {
		t.Fatalf("back to the default: %v", err)
	}
	if _, err := reopened.Accounts(ctx, "gh", "/work/app"); err != ErrNoAccounts {
		t.Fatalf("a CLI without accounts = %v", err)
	}
}

func TestAwsWhoReadsAnARN(t *testing.T) {
	for arn, want := range map[string]string{
		"arn:aws:iam::123456789012:user/deploy":                 "123456789012 · user/deploy",
		"arn:aws:sts::123456789012:assumed-role/Admin/me@x.com": "123456789012 · Admin",
		"arn:aws:iam::123456789012:root":                        "123456789012 · root",
	} {
		if got := awsWho(arn); got != want {
			t.Errorf("%s = %q, want %q", arn, got, want)
		}
	}
}

const awsConfig = `[default]
region = us-east-1
[profile repogo]
[profile fileie]
region = us-east-1
login_session = arn:aws:iam::976193265892:root

[profile clipreply]
login_session = x
region = us-west-2
`

// Removing a profile takes exactly its section; renaming touches only its
// header, in the config and, for stored keys, the credentials; each edit
// leaves a copy of the file beside it.
func TestAWSProfilesAreEditedOneSectionAtATime(t *testing.T) {
	dir := t.TempDir()
	config, credentials := filepath.Join(dir, "config"), filepath.Join(dir, "credentials")
	t.Setenv("AWS_CONFIG_FILE", config)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentials)
	if err := os.WriteFile(config, []byte(awsConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, []byte("[repogo]\naws_access_key_id = AKIA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := awsProfiles{}

	if err := p.Remove(ctx, "/usr/bin/false", "fileie"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(config)
	want := "[default]\nregion = us-east-1\n[profile repogo]\n[profile clipreply]\nlogin_session = x\nregion = us-west-2\n"
	if string(got) != want {
		t.Fatalf("after remove:\n%s", got)
	}

	if err := p.Remove(ctx, "/usr/bin/false", "nothing-here"); err != nil {
		t.Fatal(err)
	}
	if err := p.Rename(ctx, "", "repogo", "mac-host"); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(config)
	keys, _ := os.ReadFile(credentials)
	if !strings.Contains(string(got), "[profile mac-host]\n") || strings.Contains(string(got), "[profile repogo]") ||
		string(keys) != "[mac-host]\naws_access_key_id = AKIA\n" {
		t.Fatalf("after rename:\n%s\n%s", got, keys)
	}
	if info, _ := os.Stat(config); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
	backups, _ := filepath.Glob(config + ".repogo-*.bak")
	if len(backups) == 0 {
		t.Fatal("no backup beside the config")
	}

	// A profile of stored keys goes from both files.
	if err := p.Remove(ctx, "/usr/bin/false", "mac-host"); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(config)
	keys, _ = os.ReadFile(credentials)
	if strings.Contains(string(got), "mac-host") || len(keys) != 0 {
		t.Fatalf("after removing a key profile:\n%s\n%q", got, keys)
	}
}

// Renaming or removing an account moves every project's pick with it.
func TestPicksFollowARenameAndARemove(t *testing.T) {
	dir := t.TempDir()
	in := NewInventory(nil, nil)
	if err := in.OpenAccounts(filepath.Join(dir, "cli-accounts.json")); err != nil {
		t.Fatal(err)
	}
	in.accounts.picks["fake"] = map[string]string{"/a": "jerrick", "/b": "jerrick", "/c": "other"}
	if err := in.repick("fake", "jerrick", "fileie"); err != nil {
		t.Fatal(err)
	}
	if p := in.accounts.picks["fake"]; p["/a"] != "fileie" || p["/b"] != "fileie" || p["/c"] != "other" {
		t.Fatalf("after rename = %v", p)
	}
	if err := in.repick("fake", "fileie", ""); err != nil {
		t.Fatal(err)
	}
	if p := in.accounts.picks["fake"]; len(p) != 1 || p["/c"] != "other" {
		t.Fatalf("after remove = %v", p)
	}
}

// AWS's default is the profile the host itself would use: AWS_PROFILE when
// set, else "default".
func TestAWSDefaultIsTheHostsOwnProfile(t *testing.T) {
	accounts := []CLIAccount{{ID: "default"}, {ID: "fileie"}}
	t.Setenv("AWS_PROFILE", "")
	if got := (awsProfiles{}).Default(context.Background(), "", accounts); got != "default" {
		t.Fatalf("default = %q", got)
	}
	t.Setenv("AWS_PROFILE", "fileie")
	if got := (awsProfiles{}).Default(context.Background(), "", accounts); got != "fileie" {
		t.Fatalf("with AWS_PROFILE = %q", got)
	}
	t.Setenv("AWS_PROFILE", "gone")
	if got := (awsProfiles{}).Default(context.Background(), "", accounts); got != "" {
		t.Fatalf("a profile that isn't listed = %q", got)
	}
}

// A project with no pick lists nothing (a card's status asks on every draw);
// one whose pick the CLI no longer has says so.
func TestPickedAccountListsOnlyForAPick(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakecli")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	lists := 0
	in := NewInventory(nil, nil)
	in.Tools = []Tool{{Kind: "fake", Name: "Fake", Spec: Spec{Command: "fakecli", Accounts: fakeProfiles{&lists}}}}
	path := filepath.Join(dir, "cli-accounts.json")
	if err := os.WriteFile(path, []byte(`{"fake":{"/work/gone":"client-z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := in.OpenAccounts(path); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if id, _, _ := in.PickedAccount(ctx, "fake", "/work/none"); id != "" || lists != 0 {
		t.Fatalf("no pick = %q, listed %d times", id, lists)
	}
	if id, _, found := in.PickedAccount(ctx, "fake", "/work/gone"); id != "client-z" || found {
		t.Fatalf("gone pick = %q, found %v", id, found)
	}
	if err := in.SelectAccount(ctx, "fake", "/work/app", "client-b"); err != nil {
		t.Fatal(err)
	}
	if id, a, found := in.PickedAccount(ctx, "fake", "/work/app"); id != "client-b" || !found || a.ID != "client-b" {
		t.Fatalf("pick = %q, %+v, %v", id, a, found)
	}
}
