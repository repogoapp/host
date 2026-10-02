package clilogin

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeCLI prints a link in colour, then either waits for $DIR/approve (a
// device code) or reads a pasted code from stdin: "good" signs in, anything
// else fails with a last line saying so.
const fakeCLI = `#!/bin/sh
if [ "$1" = slow ]; then echo starting; while [ ! -f "$DIR/print" ]; do sleep 0.05; done; fi
printf '\033[94mhttps://example.com/authorize?x=1\033[0m\n'
if [ "$1" = paste ]; then
  printf 'Paste code here > '
  read code
  [ "$code" = good ] && { echo "Signed in"; exit 0; }
  echo "Login failed: bad code"; exit 1
fi
echo "CODE-1234"
while [ ! -f "$DIR/approve" ]; do sleep 0.05; done
echo "Signed in"
`

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func spec(t *testing.T, mode string, ended chan bool) (Spec, string) {
	t.Helper()
	dir := t.TempDir()
	cli := filepath.Join(dir, "cli")
	if err := os.WriteFile(cli, []byte(fakeCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	return Spec{
		Command: cli,
		Args:    []string{mode},
		Env:     []string{"DIR=" + dir},
		Timeout: time.Minute,
		Style:   map[bool]string{true: StylePaste, false: StyleDevice}[mode == "paste"],
		Read: func(line string, c *Code) bool {
			if strings.HasPrefix(line, "https://") {
				c.URL = line
			}
			if line == "CODE-1234" {
				c.UserCode = line
			}
			return c.URL != "" && (mode == "paste" || c.UserCode != "")
		},
		OnEnd: func(ok bool) { ended <- ok },
	}, dir
}

func waitEnd(t *testing.T, ended chan bool) bool {
	t.Helper()
	select {
	case ok := <-ended:
		return ok
	case <-time.After(10 * time.Second):
		t.Fatal("OnEnd was not called")
		return false
	}
}

func TestDeviceCodeApproved(t *testing.T) {
	l := Logins{Log: quiet()}
	ended := make(chan bool, 1)
	s, dir := spec(t, "device", ended)

	code, err := l.Start("cli", s)
	if err != nil || code.URL != "https://example.com/authorize?x=1" || code.UserCode != "CODE-1234" || code.Style != StyleDevice {
		t.Fatalf("Start = %+v, %v", code, err)
	}
	if again, _ := l.Start("cli", s); again != code {
		t.Fatalf("second Start = %+v", again)
	}
	if p := l.Pending("cli"); p == nil || *p != code {
		t.Fatalf("Pending = %+v", p)
	}
	if err := l.Complete(context.Background(), "cli", "x"); !errors.Is(err, ErrNoPaste) {
		t.Fatalf("Complete on a device code = %v", err)
	}
	os.WriteFile(filepath.Join(dir, "approve"), nil, 0o644)
	if !waitEnd(t, ended) || l.Pending("cli") != nil {
		t.Fatal("approved sign-in did not end cleanly")
	}
}

func TestPastedCodeSignsIn(t *testing.T) {
	l := Logins{Log: quiet()}
	ended := make(chan bool, 1)
	s, _ := spec(t, "paste", ended)

	code, err := l.Start("cli", s)
	if err != nil || code.Style != StylePaste || code.UserCode != "" {
		t.Fatalf("Start = %+v, %v", code, err)
	}
	if err := l.Complete(context.Background(), "cli", "  good \n"); err != nil {
		t.Fatal(err)
	}
	if !waitEnd(t, ended) || l.Pending("cli") != nil {
		t.Fatal("pasted sign-in did not end cleanly")
	}
}

func TestPastedCodeRefusedSaysWhy(t *testing.T) {
	l := Logins{Log: quiet()}
	ended := make(chan bool, 1)
	s, _ := spec(t, "paste", ended)

	if _, err := l.Start("cli", s); err != nil {
		t.Fatal(err)
	}
	err := l.Complete(context.Background(), "cli", "wrong")
	if err == nil || !strings.Contains(err.Error(), "Login failed: bad code") {
		t.Fatalf("Complete = %v", err)
	}
	if waitEnd(t, ended) {
		t.Fatal("a refused code reported success")
	}
	if err := l.Complete(context.Background(), "cli", "good"); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("Complete after the end = %v", err)
	}
}

func TestCancelWaitsOutTheGrace(t *testing.T) {
	l := Logins{Log: quiet(), Grace: 50 * time.Millisecond}
	ended := make(chan bool, 1)
	s, _ := spec(t, "device", ended)
	code, err := l.Start("cli", s)
	if err != nil {
		t.Fatal(err)
	}
	l.Cancel("cli")
	if l.Pending("cli") != nil {
		t.Fatal("a cancelled sign-in still reads as pending")
	}
	// Starting again inside the grace revives it.
	if again, _ := l.Start("cli", s); again != code {
		t.Fatalf("revived = %+v", again)
	}
	select {
	case <-ended:
		t.Fatal("the revived sign-in was cancelled")
	case <-time.After(3 * l.Grace):
	}
	if l.Pending("cli") == nil {
		t.Fatal("the revived sign-in no longer reads as pending")
	}
	l.Cancel("cli")
	if waitEnd(t, ended) {
		t.Fatal("a cancelled sign-in reported success")
	}
}

func TestNoLinkIsAnError(t *testing.T) {
	l := Logins{Log: quiet()}
	s := Spec{Command: "/bin/sh", Args: []string{"-c", "echo nothing here; exit 1"}, Timeout: time.Minute,
		Read: func(string, *Code) bool { return false }}
	if _, err := l.Start("cli", s); !errors.Is(err, ErrNoCode) {
		t.Fatalf("Start = %v", err)
	}
	if l.Pending("cli") != nil {
		t.Fatal("a failed start left a pending sign-in")
	}
}

// The phone polls Pending every second while a sign-in starts; a CLI slow to
// print its link must not hold that poll up.
func TestPendingAnswersWhileStartWaits(t *testing.T) {
	l := Logins{Log: quiet()}
	ended := make(chan bool, 1)
	s, dir := spec(t, "slow", ended)
	// The CLI's first line, before its link, says Start is under way.
	running := make(chan struct{})
	read := s.Read
	s.Read = func(line string, c *Code) bool {
		if line == "starting" {
			close(running)
		}
		return read(line, c)
	}
	started := make(chan error, 1)
	go func() {
		_, err := l.Start("cli", s)
		started <- err
	}()

	answered := make(chan *Code, 1)
	go func() {
		<-running
		answered <- l.Pending("cli")
	}()
	select {
	case code := <-answered:
		if code != nil {
			t.Fatalf("a sign-in with no link yet reads as pending: %+v", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pending blocked behind Start")
	}

	if err := os.WriteFile(filepath.Join(dir, "print"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if l.Pending("cli") == nil {
		t.Fatal("started sign-in is not pending")
	}
	l.Cancel("cli")
	os.WriteFile(filepath.Join(dir, "approve"), nil, 0o644)
	waitEnd(t, ended)
}

// A CLI that refuses to sign in without a terminal (flyctl) prints its link
// when run on one.
func TestTTYSignInRunsOnATerminal(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "cli")
	script := "#!/bin/sh\n[ -t 0 ] || { echo 'requires an interactive terminal'; exit 1; }\n" +
		"echo 'Opening https://example.com/cli/abc ...'\nwhile [ ! -f \"$DIR/approve\" ]; do sleep 0.05; done\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ended := make(chan bool, 1)
	s := Spec{
		Command: cli, Env: []string{"DIR=" + dir}, Timeout: time.Minute, TTY: true,
		Read: func(line string, c *Code) bool {
			if i := strings.Index(line, "https://"); i >= 0 {
				c.URL = strings.TrimSuffix(line[i:], " ...")
			}
			return c.URL != ""
		},
		OnEnd: func(ok bool) { ended <- ok },
	}
	l := Logins{Log: quiet()}
	code, err := l.Start("cli", s)
	if err != nil || code.URL != "https://example.com/cli/abc" {
		t.Fatalf("Start = %+v, %v", code, err)
	}
	os.WriteFile(filepath.Join(dir, "approve"), nil, 0o644)
	if !waitEnd(t, ended) {
		t.Fatal("the sign-in on a terminal did not end cleanly")
	}
}

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

// A callback sign-in whose port is taken says so before the CLI starts.
func TestCallbackPortInUseIsNamed(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ended := make(chan bool, 1)
	s, _ := spec(t, "device", ended)
	s.Port = ln.Addr().(*net.TCPAddr).Port
	l := Logins{Log: quiet()}
	if _, err := l.Start("cli", s); !errors.Is(err, ErrPortInUse) {
		t.Fatalf("Start = %v, want ErrPortInUse", err)
	}
	if l.Pending("cli") != nil {
		t.Fatal("a refused start left a pending sign-in")
	}
}

// Close stops a waiting sign-in at once, as the host shuts down.
func TestCloseStopsWaitingSignIns(t *testing.T) {
	ended := make(chan bool, 1)
	s, _ := spec(t, "device", ended)
	l := Logins{Log: quiet()}
	if _, err := l.Start("cli", s); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if waitEnd(t, ended) {
		t.Fatal("a sign-in stopped at shutdown reported success")
	}
	if l.Pending("cli") != nil {
		t.Fatal("still pending after Close")
	}
}
