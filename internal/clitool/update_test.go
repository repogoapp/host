package clitool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdateOfASudoNPMInstallSaysWhatToRun(t *testing.T) {
	home, bin := fakeHome(t)
	// The host resolves the binary's links, and a temp dir can sit behind one.
	real, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(real, "usr")
	pkg := filepath.Join(prefix, "lib/node_modules/fakecli")
	writeScript(t, filepath.Join(pkg, "cli.js"), fakeCLI)
	if err := os.Symlink(filepath.Join(pkg, "cli.js"), filepath.Join(bin, "fakecli")); err != nil {
		t.Fatal(err)
	}
	// npm would fail here; the update must not reach it.
	writeScript(t, filepath.Join(bin, "npm"), "exit 243\n")
	modules := filepath.Dir(pkg)
	if err := os.Chmod(modules, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(modules, 0o755) })
	if writable(modules) {
		t.Skip("running as a user who can write anywhere")
	}
	in := inventoryOf(Tool{Kind: "fake", Name: "Fake", Spec: Spec{Command: "fakecli", Pkg: "fakecli"}})
	in.latest["fakecli"] = latestEntry{version: "9.9.9", expiresAt: time.Now().Add(time.Hour)}

	res := in.Update(t.Context(), "fake")
	want := "sudo " + filepath.Join(prefix, "bin/npm") + " install -g fakecli@latest"
	if res.OK || res.Ran != "" || !strings.Contains(res.Error, modules) || !strings.HasSuffix(res.Error, want) {
		t.Fatalf("Update = %+v, want an error naming %s and ending %q", res, modules, want)
	}
}

func TestFailureLineIsTheReasonNotTheExitCode(t *testing.T) {
	npm := `npm error code EACCES
npm error Error: EACCES: permission denied, rename '/usr/local/lib/node_modules/vercel'
npm error     at async Object.rename (node:internal/fs/promises:786:10)
npm error A complete log of this run can be found in: /tmp/debug.log`
	for out, want := range map[string]string{
		npm:                                  "EACCES: permission denied, rename '/usr/local/lib/node_modules/vercel'",
		"Updating...\nnetwork unreachable\n": "network unreachable",
		"":                                   "",
	} {
		if got := failureLine(out); got != want {
			t.Errorf("failureLine(%q) = %q, want %q", out, got, want)
		}
	}
}
