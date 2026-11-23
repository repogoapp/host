package remotehost

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The remote command runs the serving host's own binary when it recorded
// one, else the release's, and answers 127 when there is neither, as the
// login shell that SSH hands it to would run it.
func TestRepogoRunsTheServingHostsBinary(t *testing.T) {
	home := t.TempDir()
	stub := func(path, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+name+" \"$@\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run := func() (string, int) {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", Repogo("status --json"))
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(out), exit.ExitCode()
		}
		return strings.TrimSpace(string(out)), 0
	}

	if _, code := run(); code != 127 {
		t.Fatalf("with no repogo: exit %d, want 127", code)
	}
	stub(filepath.Join(home, ".repogo", "bin", "repogo"), "release")
	if out, _ := run(); out != "release status --json" {
		t.Fatalf("release only: %q", out)
	}
	dev := filepath.Join(home, "repo go-dev", "repogo")
	stub(dev, "dev")
	if err := os.WriteFile(filepath.Join(home, ".repogo", HostBinFile), []byte(dev+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _ := run(); out != "dev status --json" {
		t.Fatalf("a development host recorded: %q", out)
	}
	if err := os.Remove(dev); err != nil {
		t.Fatal(err)
	}
	if out, _ := run(); out != "release status --json" {
		t.Fatalf("a recorded binary that's gone: %q", out)
	}
}
