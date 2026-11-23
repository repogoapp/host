package remotehost

import (
	"context"
	"reflect"
	"testing"
)

func TestParseSSHConfigTakesEachHostsFirstName(t *testing.T) {
	config := []byte(`
# comment
Host *
  ServerAliveInterval 30
Host studio Servers-Mac-Studio.local
  HostName 100.73.205.82
  User server
host=newmac
Match all
Host !bad *.example.com
Host github-personal
`)
	got := parseSSHConfig(config)
	want := []string{"studio", "newmac", "github-personal"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aliases %v, want %v", got, want)
	}
}

func TestParseTailnetKeepsComputersInNameOrder(t *testing.T) {
	status := []byte(`{"Self": {"DNSName": "me.ts.net.", "OS": "macOS"}, "Peer": {
	  "k1": {"DNSName": "studio.t.ts.net.", "OS": "macOS", "Online": true, "TailscaleIPs": ["100.73.205.82"]},
	  "k2": {"DNSName": "iphone.t.ts.net.", "OS": "iOS", "Online": true},
	  "k3": {"DNSName": "box.t.ts.net.", "OS": "linux", "Online": false}}}`)
	got := parseTailnet(status)
	if len(got) != 2 || got[0].DNSName != "box.t.ts.net." || got[1].DNSName != "studio.t.ts.net." {
		t.Fatalf("peers %+v, want box then studio, no iPhone and no self", got)
	}
	if parseTailnet(nil) != nil || parseTailnet([]byte("not json")) != nil {
		t.Fatal("unreadable tailnet output gave peers")
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
		{1, "repogo: host is not running; run repogo start", NotRunning},
		{1, "repogo: unknown flag --json for repogo status; run repogo help status", TooOld},
		{-1, "", Offline},
	} {
		if got, _ := classify(tc.code, tc.stderr); got != tc.want {
			t.Errorf("classify(%d, %q) = %s, want %s", tc.code, tc.stderr, got, tc.want)
		}
	}
}

// fake is a machine with ~/.ssh/config, a tailnet and SSH answers by target.
type fake struct {
	config  string
	resolve map[string]string
	tailnet string
	answers map[string]answer
	ran     []string
}

type answer struct {
	stdout, stderr string
	code           int
}

func (f *fake) deps(self string) Deps {
	return Deps{
		Self:      self,
		SSHConfig: func() []byte { return []byte(f.config) },
		Resolve:   func(_ context.Context, alias string) string { return f.resolve[alias] },
		Tailnet:   func(context.Context) []byte { return []byte(f.tailnet) },
		Run: func(_ context.Context, target, command string) ([]byte, []byte, int) {
			f.ran = append(f.ran, target)
			if command != statusCommand {
				return nil, []byte("unexpected command " + command), 1
			}
			a, ok := f.answers[target]
			if !ok {
				return nil, []byte("ssh: Could not resolve hostname " + target), 255
			}
			return []byte(a.stdout), []byte(a.stderr), a.code
		},
	}
}

func TestDiscoverMergesOneMachineAndSaysWhyOthersCantBeUsed(t *testing.T) {
	f := &fake{
		config:  "Host studio\nHost newmac\nHost old\nHost github-work\n",
		resolve: map[string]string{"studio": "100.73.205.82", "newmac": "100.105.9.117", "old": "192.168.1.9", "github-work": "github.com"},
		tailnet: `{"Peer": {
		  "a": {"DNSName": "servers-mac-studio.t.ts.net.", "OS": "macOS", "Online": true, "TailscaleIPs": ["100.73.205.82"]},
		  "b": {"DNSName": "jerricks-macbook-pro.t.ts.net.", "OS": "macOS", "Online": false, "TailscaleIPs": ["100.105.9.117"]},
		  "c": {"DNSName": "linuxbox.t.ts.net.", "OS": "linux", "Online": true, "TailscaleIPs": ["100.64.0.9"]},
		  "d": {"DNSName": "me.t.ts.net.", "OS": "macOS", "Online": true, "TailscaleIPs": ["100.64.0.1"]}}}`,
		answers: map[string]answer{
			"studio":            {stdout: `{"host_id": "H-STUDIO", "name": "Server's Mac Studio", "version": "0.1.50"}`},
			"old":               {stderr: "repogo: unknown flag --json for repogo status", code: 1},
			"linuxbox.t.ts.net": {stderr: "user@linuxbox: Permission denied (publickey).", code: 255},
			"me.t.ts.net":       {stdout: `{"host_id": "H-SELF", "name": "This one"}`},
		},
	}
	got := Discover(context.Background(), f.deps("H-SELF"))
	want := []Host{
		{ID: "studio", HostID: "H-STUDIO", Name: "Server's Mac Studio", Version: "0.1.50", State: Ready},
		{ID: "newmac", State: Offline, Reason: "offline in Tailscale"},
		{ID: "old", State: TooOld, Reason: "update RepoGo there"},
		{ID: "linuxbox", State: NoKey, Reason: "no SSH key that works there"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover:\n got %+v\nwant %+v", got, want)
	}
	for _, target := range f.ran {
		if target == "newmac" || target == "github-work" || target == "servers-mac-studio.t.ts.net" || target == "jerricks-macbook-pro.t.ts.net" {
			t.Errorf("called SSH on %s: an offline machine or one ~/.ssh/config already dials", target)
		}
	}
}

func TestMergeKeepsTheFirstNameForAHost(t *testing.T) {
	got := merge([]Host{
		{ID: "studio", HostID: "H1", State: Ready},
		{ID: "servers-mac-studio", HostID: "H1", State: Ready},
		{ID: "x", State: Offline},
		{ID: "y", State: Offline},
	}, "")
	if len(got) != 3 || got[0].ID != "studio" || got[1].ID != "x" || got[2].ID != "y" {
		t.Fatalf("merge = %+v, want studio, x, y", got)
	}
}
