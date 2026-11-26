package remotehost

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Only a name SSH would dial as that alias is listed: nothing it would read
// as an option, a user, a URI or two words, and nothing guessed from a line
// that doesn't parse.
func TestParseSSHConfigNamesEachConnectionOnce(t *testing.T) {
	config := []byte("# comment\n" +
		"Host *\n  ServerAliveInterval 30\n" +
		"Host studio Servers-Mac-Studio.local\n  HostName 100.73.205.82\n  User server\n  IdentityFile ~/.ssh/id_studio\n" +
		"host=newmac\n" +
		"Host\ttabbed # the lab box\n\tHostName\tlab.example.com\n" +
		"Host \"quoted\"\n" +
		"Match all\n  HostName wrong.example.com\n" +
		"Host !bad *.example.com\n  HostName also-wrong.example.com\n" +
		"Host studio\n  HostName second.example.com\n" +
		"Host github-personal\n  HostName github.com\n" +
		"Host gitlab.com\n" +
		"Host -oProxyCommand=evil\n" +
		"Host root@elsewhere ssh://elsewhere:22 \"two words\"\n" +
		"Host \"unclosed\n" +
		"Host plain\r\n" +
		"Host " + strings.Repeat("x", 2<<20) + "\n" +
		"Host after-a-line-too-long\n")
	got := parseSSHConfig(config)
	want := []candidate{
		{id: "studio", address: "100.73.205.82"},
		{id: "newmac"},
		{id: "tabbed", address: "lab.example.com"},
		{id: "quoted"},
		{id: "plain"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aliases %+v\n   want %+v", got, want)
	}
}

func TestClassifyReadsHowTheCallFailed(t *testing.T) {
	for _, tc := range []struct {
		code   int
		stderr string
		want   State
	}{
		{0, "", Ready},
		{255, "server@studio: Permission denied (publickey).", NoKey},
		{255, "Host key verification failed.", NoKey},
		{255, "ssh: connect to host studio port 22: Operation timed out", Offline},
		{255, "ssh: Could not resolve hostname nope", Offline},
		{127, "zsh:1: no such file or directory: /Users/server/.repogo/bin/repogo", NotInstalled},
		{ExitNotRunning, "repogo: whatever it says", NotRunning},
		{1, "repogo: unknown flag --json for repogo status; run repogo help status", TooOld},
		{-1, "", Offline},
	} {
		if got, _ := classify(tc.code, tc.stderr, "studio"); got != tc.want {
			t.Errorf("classify(%d, %q) = %s, want %s", tc.code, tc.stderr, got, tc.want)
		}
	}
	// A refused sign-in doesn't claim to know why, and names what to try.
	if _, why := classify(255, "Permission denied", "studio"); !strings.Contains(why, "ssh studio") || strings.Contains(why, "key") {
		t.Errorf("sign-in reason %q, want it to name ssh studio and blame neither the key nor the username", why)
	}
}

// fake is a machine with ~/.ssh/config and SSH answers by alias.
type fake struct {
	config  string
	answers map[string]answer
	// mu guards what follows: a look checks its hosts at once.
	mu     sync.Mutex
	read   int
	ran    []string
	closed []string
}

type answer struct {
	stdout, stderr string
	code           int
}

func (f *fake) deps(self string) Deps {
	return Deps{
		Self: self,
		SSHConfig: func() []byte {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.read++
			return []byte(f.config)
		},
		Changed: func() {},
		Close: func(_ context.Context, alias string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.closed = append(f.closed, alias)
		},
		Run: func(_ context.Context, alias, command string) ([]byte, []byte, int) {
			f.mu.Lock()
			f.ran = append(f.ran, alias)
			f.mu.Unlock()
			if command != statusCommand {
				return nil, []byte("unexpected command " + command), 1
			}
			a, ok := f.answers[alias]
			if !ok {
				return nil, []byte("ssh: Could not resolve hostname " + alias), 255
			}
			return []byte(a.stdout), []byte(a.stderr), a.code
		},
	}
}

// everyHost is a user who switched every host on.
func everyHost(string) bool { return true }

func TestDiscoverChecksEachHostAndSaysWhyOneCantBeUsed(t *testing.T) {
	f := &fake{
		config: "Host studio\n  HostName 100.73.205.82\nHost old\nHost linuxbox\nHost me\nHost gone\n",
		answers: map[string]answer{
			"studio":   {stdout: `{"host_id": "H-STUDIO", "name": "What it calls itself", "version": "0.1.50"}`},
			"old":      {stderr: "repogo: unknown flag --json for repogo status", code: 1},
			"linuxbox": {stderr: "user@linuxbox: Permission denied (publickey).", code: 255},
			"me":       {stdout: `{"host_id": "H-SELF", "name": "This one"}`},
		},
	}
	got := Discover(context.Background(), f.deps("H-SELF"), everyHost)
	_, signIn := classify(255, "Permission denied", "linuxbox")
	want := []Host{
		{ID: "studio", HostID: "H-STUDIO", Version: "0.1.50", State: Ready, Address: "100.73.205.82"},
		{ID: "old", State: TooOld, Reason: "update RepoGo there"},
		{ID: "linuxbox", State: NoKey, Reason: signIn},
		{ID: "me", HostID: "H-SELF", State: Self, Reason: "this is the environment you're on"},
		{ID: "gone", State: Offline, Reason: "ssh: Could not resolve hostname gone"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover:\n got %+v\nwant %+v", got, want)
	}
}

// A host the user hasn't switched on is named from the file and never dialed.
func TestDiscoverDialsOnlyTheHostsSwitchedOn(t *testing.T) {
	f := &fake{
		config:  "Host studio\nHost work\n  HostName db.prod.example.com\n",
		answers: map[string]answer{"studio": {stdout: `{"host_id": "H-STUDIO"}`}},
	}
	got := Discover(context.Background(), f.deps(""), func(alias string) bool { return alias == "studio" })
	want := []Host{
		{ID: "studio", HostID: "H-STUDIO", State: Ready},
		{ID: "work", State: Off, Address: "db.prod.example.com"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover:\n got %+v\nwant %+v", got, want)
	}
	if !reflect.DeepEqual(f.ran, []string{"studio"}) {
		t.Fatalf("dialed %v, want only studio", f.ran)
	}
}

// An id that is nothing once cleaned is no id: the host didn't say who it is.
func TestAHostIDThatCleansToNothingIsNoAnswer(t *testing.T) {
	f := &fake{config: "Host box\n", answers: map[string]answer{"box": {stdout: `{"host_id": "\u0001", "version": "9"}`}}}
	got := Discover(context.Background(), f.deps(""), everyHost)
	if len(got) != 1 || got[0].State != TooOld || got[0].HostID != "" || got[0].Version != "" {
		t.Fatalf("Discover = %+v, want box too old with no id", got)
	}
}

// What another host writes is cut short and loses escapes and invisible
// characters before it reaches an agent or a terminal.
func TestAnotherHostsTextIsCleaned(t *testing.T) {
	f := &fake{config: "Host evil\n", answers: map[string]answer{
		"evil": {stdout: `{"host_id": "H9\u001b[2J‮` + strings.Repeat("x", 300) + `", "version": "1.0\n2.0"}`},
	}}
	got := Discover(context.Background(), f.deps(""), everyHost)
	if len(got) != 1 {
		t.Fatalf("Discover = %+v", got)
	}
	id := got[0].HostID
	if strings.ContainsAny(id, "\x1b‮\n") || len([]rune(id)) > 65 || !strings.HasPrefix(id, "H9[2Jx") {
		t.Fatalf("host id %q, want it printable and at most 64 runes", id)
	}
	if got[0].Version != "1.02.0" {
		t.Fatalf("version %q", got[0].Version)
	}
	if line := firstLine("ssh: \x1b]0;owned\x07 failed\n" + strings.Repeat("y", 500)); strings.ContainsRune(line, 0x1b) || len(line) > 200 {
		t.Fatalf("error line %q", line)
	}
}

// One SSH call keeps at most its cap of output and drops the rest.
func TestOutputIsCapped(t *testing.T) {
	c := &capped{max: 4}
	n, err := c.Write([]byte("abcdef"))
	c.Write([]byte("gh"))
	if n != 6 || err != nil || c.String() != "abcd" {
		t.Fatalf("kept %q after %d, %v", c.String(), n, err)
	}
}
