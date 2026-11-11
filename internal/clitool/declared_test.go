package clitool

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogoapp/host/internal/clilogin"
)

func gcloud(t *testing.T) Tool {
	t.Helper()
	for _, d := range DeclaredCLIs {
		if d.Kind == "gcloud" {
			return d.Tool()
		}
	}
	t.Fatal("no gcloud")
	return Tool{}
}

// gcloud's --no-launch-browser sign-in prints the page, then waits for the
// code it shows: the reader is done at the page, and the step is a paste.
func TestGcloudReadsItsSignInPage(t *testing.T) {
	tool := gcloud(t)
	if tool.Spec.LoginStyle != clilogin.StylePaste || tool.Category != CategoryCloud {
		t.Fatalf("tool = %+v", tool)
	}
	var c clilogin.Code
	for _, line := range []string{
		"Go to the following link in your browser, and complete the sign-in prompts:",
		"",
		"    https://accounts.google.com/o/oauth2/auth?response_type=code&client_id=32555940559.apps.googleusercontent.com&scope=openid",
		"",
		"Once finished, enter the verification code provided in your browser:",
	} {
		if tool.Spec.LoginRead(line, &c) {
			break
		}
	}
	if want := "https://accounts.google.com/o/oauth2/auth?response_type=code&client_id=32555940559.apps.googleusercontent.com&scope=openid"; c.URL != want {
		t.Fatalf("url = %q", c.URL)
	}
}

// A declared code pattern holds the reader until the code shows too, and a
// default page stands in for a CLI that prints only the code.
func TestADeclaredReaderWaitsForItsCode(t *testing.T) {
	read := readLine(nil, compile(`code: ([A-Z0-9-]+)`), "https://example.com/device")
	var c clilogin.Code
	if read("Opening a browser…", &c) {
		t.Fatal("done before the code")
	}
	if !read("Your code: ABCD-1234", &c) || c.URL != "https://example.com/device" || c.UserCode != "ABCD-1234" {
		t.Fatalf("code = %+v", c)
	}
}

// The status command's first line is the account; a blank answer or a
// failure is signed out. Its answer stands for statusTTL unless cleared.
func TestADeclaredStatusIsCachedUntilCleared(t *testing.T) {
	dir := t.TempDir()
	answer := filepath.Join(dir, "answer")
	cli := filepath.Join(dir, "cli")
	script := "#!/bin/sh\ncat " + answer + "\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		if err := os.WriteFile(answer, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	status := &statusCache{}
	ctx := context.Background()

	write("me@example.com\nother@example.com\n")
	if ok, _, a := status.probe(ctx, cli); !ok || a == nil || a.Email != "me@example.com" {
		t.Fatalf("signed in = %v, %+v", ok, a)
	}
	write("")
	if ok, _, _ := status.probe(ctx, cli); !ok {
		t.Fatal("answered again within its TTL")
	}
	status.clear()
	if ok, _, a := status.probe(ctx, cli); ok || a != nil {
		t.Fatalf("after a sign-out = %v, %+v", ok, a)
	}
	status.clear()
	write("octocat\n")
	if _, _, a := status.probe(ctx, cli); a == nil || a.Login != "octocat" {
		t.Fatalf("a handle = %+v", a)
	}
}

// Firebase's login:list exits 0 signed out too: only its "Logged in as"
// line is a sign-in.
func TestStatusPatternFindsTheAccount(t *testing.T) {
	s := &statusCache{pattern: compile(`^Logged in as (\S+@\S+)`)}
	if got := s.who("Logged in as a@b.com\n\nOther available accounts\n - c@d.com\n"); got != "a@b.com" {
		t.Fatalf("who = %q", got)
	}
	if got := s.who("⚠  No authorized accounts, run \"firebase login\"\n"); got != "" {
		t.Fatalf("signed out read as %q", got)
	}
}
