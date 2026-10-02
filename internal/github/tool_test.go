package github

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/binfetch"
	"github.com/repogo/host/internal/clilogin"
	"github.com/repogo/host/internal/clitool"
)

// gh 2.57's stderr with no terminal attached, captured 09-25.
const capturedLogin = `
! First copy your one-time code: 1DB7-43CE
Open this URL to continue in your web browser: https://github.com/login/device
`

// gh 2.101's, captured on the CI runner 09-25: the code moved into brackets.
const capturedLogin2101 = `
! One-time code (05F2-7300) copied to clipboard
Open this URL to continue in your web browser: https://github.com/login/device
`

func TestReadDeviceCodeFromCapturedOutput(t *testing.T) {
	read := func(captured string) clilogin.Code {
		var c clilogin.Code
		for _, line := range strings.Split(captured, "\n") {
			if readDeviceLine(strings.TrimSpace(line), &c) {
				break
			}
		}
		return c
	}
	for captured, want := range map[string]string{capturedLogin: "1DB7-43CE", capturedLogin2101: "05F2-7300"} {
		if c := read(captured); c.UserCode != want || c.URL != "https://github.com/login/device" {
			t.Fatalf("got %+v, want %s", c, want)
		}
	}
	if c := read("error: something else\n"); c.UserCode != "" {
		t.Fatalf("found a code in %+v", c)
	}
}

// fakeGH prints a device code for `auth login`, then waits for the test to
// approve (exit 0) or for a kill. It records `auth setup-git`.
const fakeGH = `#!/bin/sh
case "$1 $2" in
"auth login")
  echo "! First copy your one-time code: ABCD-1234" >&2
  echo "Open this URL to continue in your web browser: https://github.com/login/device" >&2
  while [ ! -f "$FAKE_GH/approve" ]; do sleep 0.05; done
  exit 0 ;;
"auth setup-git") echo setup-git >> "$FAKE_GH/log"; exit 0 ;;
*) exit 1 ;;
esac
`

// inventory runs only gh's tool, against a fake gh first on PATH, and
// reports each change on the returned channel.
func inventory(t *testing.T) (*clitool.Inventory, string, chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH", dir)
	changed := make(chan struct{}, 4)
	in := clitool.NewInventory(func() { changed <- struct{}{} }, slog.New(slog.DiscardHandler))
	in.Tools = []clitool.Tool{newService(t, t.TempDir()).Tool()}
	return in, dir, changed
}

func TestSignInPointsGitAtGHsToken(t *testing.T) {
	in, dir, changed := inventory(t)

	code, err := in.LoginStart(t.Context(), "github")
	if err != nil || code.UserCode != "ABCD-1234" || code.URL != deviceURL {
		t.Fatalf("LoginStart = %+v, %v", code, err)
	}
	if waiting := in.Logins(); len(waiting) != 1 || waiting[0] != "github" {
		t.Fatalf("waiting = %v", waiting)
	}

	if err := os.WriteFile(filepath.Join(dir, "approve"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("the end of the sign-in was not reported")
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "log")); !strings.Contains(string(log), "setup-git") {
		t.Fatal("git was not pointed at gh's token")
	}
	if waiting := in.Logins(); len(waiting) != 0 {
		t.Fatalf("still waiting: %v", waiting)
	}
}

func TestGHIsSourceControlAndSignedOutWhenGitHubSaysNo(t *testing.T) {
	in, _, _ := inventory(t)
	gh, ok := in.Get(t.Context(), "github")
	if !ok || !gh.Installed || gh.Authed || gh.Category != clitool.CategorySourceControl {
		t.Fatalf("gh = %+v", gh)
	}
}

func TestGHAssetNames(t *testing.T) {
	for sys, want := range map[[2]string]string{
		{"linux", "arm64"}:  "gh_2.101.0_linux_arm64.tar.gz",
		{"linux", "amd64"}:  "gh_2.101.0_linux_amd64.tar.gz",
		{"darwin", "arm64"}: "gh_2.101.0_macOS_arm64.zip",
	} {
		name, err := ghAsset(sys[0], sys[1])
		if err != nil || fmt.Sprintf(name, "2.101.0") != want {
			t.Fatalf("%v: %q, %v", sys, name, err)
		}
	}
	if _, err := ghAsset("windows", "amd64"); !errors.Is(err, binfetch.ErrUnsupported) {
		t.Fatalf("windows: %v", err)
	}
}
