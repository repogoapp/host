package clitool

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fakeHome confines HOME and PATH to a temp dir with an empty bin on PATH,
// so only the CLIs a test writes are found.
func fakeHome(t *testing.T) (home, bin string) {
	t.Helper()
	home = t.TempDir()
	bin = filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	return home, bin
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func inventoryOf(tools ...Tool) *Inventory {
	in := NewInventory(nil, slog.New(slog.DiscardHandler))
	in.Tools = tools
	return in
}

// fakeCLI answers --version and, for logout, deletes its credentials file.
const fakeCLI = `case "$1" in
--version) echo "fakecli 1.2.3" ;;
logout) rm -f "$HOME/.fake/creds" ;;
esac
`

func TestLogoutRunsTheCLIsSignOut(t *testing.T) {
	home, bin := fakeHome(t)
	writeScript(t, filepath.Join(bin, "fakecli"), fakeCLI)
	cfg := filepath.Join(home, ".fake")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "creds"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := inventoryOf(Tool{Kind: "fake", Name: "Fake", Home: cfg, Spec: Spec{
		Command: "fakecli", AuthPaths: []string{"creds"}, LogoutArgs: []string{"logout"},
	}})

	before, _ := in.Get(t.Context(), "fake")
	if !before.Authed || !slices.Contains(before.Capabilities, CapabilityLogout) {
		t.Fatalf("before: authed %v, capabilities %v", before.Authed, before.Capabilities)
	}
	res, err := in.Logout(t.Context(), "fake")
	if err != nil || !res.OK {
		t.Fatalf("Logout = %+v, %v", res, err)
	}
	after, _ := in.Get(t.Context(), "fake")
	if after.Authed || slices.Contains(after.Capabilities, CapabilityLogout) {
		t.Fatalf("after: authed %v, capabilities %v", after.Authed, after.Capabilities)
	}
}

func TestLogoutWithoutASignOutIsUnknown(t *testing.T) {
	fakeHome(t)
	in := inventoryOf(Tool{Kind: "fake", Spec: Spec{Command: "fakecli"}})
	if _, err := in.Logout(t.Context(), "fake"); err == nil {
		t.Fatal("Logout of a tool with no sign-out succeeded")
	}
}

func TestUninstallRemovesTheNativeBinaryAndKeepsConfig(t *testing.T) {
	home, _ := fakeHome(t)
	native := filepath.Join(home, ".local/bin/fakecli")
	writeScript(t, native, fakeCLI)
	cfg := filepath.Join(home, ".fake")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	in := inventoryOf(Tool{Kind: "fake", Name: "Fake", Home: cfg, Spec: Spec{
		Command: "fakecli", NativePaths: []string{".local/bin/fakecli"},
	}})

	before, _ := in.Get(t.Context(), "fake")
	if before.Manager != "native" || !slices.Contains(before.Capabilities, CapabilityUninstall) {
		t.Fatalf("before: manager %q, capabilities %v", before.Manager, before.Capabilities)
	}
	res, err := in.Uninstall(t.Context(), "fake")
	if err != nil || !res.OK {
		t.Fatalf("Uninstall = %+v, %v", res, err)
	}
	if _, err := os.Stat(native); !os.IsNotExist(err) {
		t.Fatalf("binary still there: %v", err)
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("config directory removed: %v", err)
	}
}

func TestUninstallRunsTheManagersRemove(t *testing.T) {
	home, bin := fakeHome(t)
	cli := filepath.Join(home, "homebrew/bin/fakecli")
	writeScript(t, cli, fakeCLI)
	t.Setenv("PATH", bin+":"+filepath.Dir(cli)+":/usr/bin:/bin")
	// brew uninstall <formula> removes the formula's binary.
	writeScript(t, filepath.Join(bin, "brew"), `[ "$1 $2" = "uninstall fakecli" ] && rm -f "`+cli+`"`+"\n")
	in := inventoryOf(Tool{Kind: "fake", Name: "Fake", Spec: Spec{Command: "fakecli", Formula: "fakecli"}})

	res, err := in.Uninstall(t.Context(), "fake")
	if err != nil || !res.OK || res.Ran != "brew uninstall fakecli" {
		t.Fatalf("Uninstall = %+v, %v", res, err)
	}
	if after, _ := in.Get(t.Context(), "fake"); after.Installed {
		t.Fatalf("still installed at %s", after.Path)
	}
}

func TestUninstallRefusesAnUnknownInstall(t *testing.T) {
	_, bin := fakeHome(t)
	cli := filepath.Join(bin, "fakecli")
	writeScript(t, cli, fakeCLI)
	in := inventoryOf(Tool{Kind: "fake", Name: "Fake", Spec: Spec{Command: "fakecli"}})

	before, _ := in.Get(t.Context(), "fake")
	if slices.Contains(before.Capabilities, CapabilityUninstall) {
		t.Fatalf("an unknown install offers uninstall: %v", before.Capabilities)
	}
	res, err := in.Uninstall(t.Context(), "fake")
	if err != nil || res.OK || res.Error == "" {
		t.Fatalf("Uninstall = %+v, %v", res, err)
	}
	if _, err := os.Stat(cli); err != nil {
		t.Fatalf("binary removed: %v", err)
	}
}
