package remotehost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/errkind"
)

// studioAt is a service that lists one connection, studio, answering with
// hostID, its choices saved at path.
func studioAt(t *testing.T, path, hostID string) (*Service, *fake, *int) {
	t.Helper()
	f := &fake{config: "Host studio\n", answers: map[string]answer{"studio": {stdout: `{"host_id": "` + hostID + `"}`}}}
	d := f.deps("")
	d.Send = (&sender{}).send
	d.AuthorizedPath = path
	changes := 0
	d.Changed = func() { changes++ }
	return NewService(d), f, &changes
}

// Until the user switches it on, a host is neither dialed nor called.
func TestAHostIsntDialedUntilTheUserSwitchesItOn(t *testing.T) {
	s := &sender{answers: []answer{{stdout: `{"result": 1}`}}}
	svc, f := listedStudio(t, s)
	if got := svc.List(context.Background(), false).Hosts[0]; got.Authorized || got.State != Off || got.Address != "100.73.205.82" {
		t.Fatalf("listed %+v, want studio off with its address", got)
	}
	if len(f.ran)+len(s.targets) != 0 {
		t.Fatalf("dialed %v and %v before the user switched studio on", f.ran, s.targets)
	}
	on, err := svc.Authorize(context.Background(), "studio", true)
	if err != nil || !on.Authorized || on.State != Ready || on.HostID != "H1" || !svc.List(context.Background(), false).Hosts[0].Authorized {
		t.Fatalf("Authorize = %+v, %v; want studio on and ready, in the list too", on, err)
	}
	if !slices.Equal(f.ran, []string{"studio"}) {
		t.Fatalf("dialed %v, want only studio", f.ran)
	}
	if _, err := svc.Call(context.Background(), "studio", Request{}, true); err != nil {
		t.Fatal(err)
	}
	off, err := svc.Authorize(context.Background(), "studio", false)
	if err != nil || off.Authorized || off.State != Off || off.HostID != "" {
		t.Fatalf("Authorize off = %+v, %v; want studio off with nothing it answered", off, err)
	}
	if _, err := svc.Call(context.Background(), "studio", Request{}, true); !errors.Is(err, errkind.ErrDenied) {
		t.Fatalf("Call after switching off = %v, want denied", err)
	}
}

func TestChoicesAreSavedPrivateAndTellDevices(t *testing.T) {
	path := filepath.Join(t.TempDir(), AuthorizedFile)
	svc, _, changes := studioAt(t, path, "H1")
	for range 2 {
		if _, err := svc.SetListing(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Authorize(context.Background(), "studio", true); err != nil {
			t.Fatal(err)
		}
	}
	if *changes != 2 {
		t.Fatalf("%d hosts.changed for the list and one switch, each flipped twice the same way; want 2", *changes)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved file: %v, %v; want mode 0600", info, err)
	}

	// A restart reads it back.
	again, _, _ := studioAt(t, path, "H1")
	if got := again.List(context.Background(), true); !got.Listing || !got.Hosts[0].Authorized || got.Hosts[0].State != Ready {
		t.Fatalf("after a restart %+v, want the list on and studio still on", got)
	}
	// The same alias answering with another host ID isn't the host the user
	// picked: it is off again, and says why once.
	swapped, f, _ := studioAt(t, path, "H2")
	got := swapped.List(context.Background(), true).Hosts[0]
	if got.Authorized || got.State != Off || got.HostID != "" || !strings.Contains(got.Reason, "another environment answers there") {
		t.Fatalf("%+v: want a new host ID at studio's alias off, with why", got)
	}
	if len(f.closed) != 1 {
		t.Fatalf("closed %v, want its login ended", f.closed)
	}
	if _, err := swapped.Call(context.Background(), "studio", Request{}, true); !errors.Is(err, errkind.ErrDenied) {
		t.Fatalf("Call to the swapped host = %v, want denied", err)
	}
	if back, _, _ := studioAt(t, path, "H1"); back.List(context.Background(), true).Hosts[0].Authorized {
		t.Fatal("the swapped host was switched off, but the file still has studio on")
	}
}

func TestAFileThatCantBeReadChoosesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), AuthorizedFile)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, f, _ := studioAt(t, path, "H1")
	if got := svc.List(context.Background(), true); got.Listing || len(got.Hosts) != 0 || f.read != 0 {
		t.Fatalf("%+v after reading the SSH file %d times, want nothing listed or read", got, f.read)
	}
}

// The switch is the user's yes to dialing a host, so one without RepoGo yet
// can be switched on: it says what's missing, and is ready once that's fixed.
func TestAHostThatCantBeUsedYetCanBeSwitchedOn(t *testing.T) {
	svc, f := listedStudio(t, &sender{})
	f.answers["work"] = answer{stderr: "sh: repogo: not found", code: 127}
	on, err := svc.Authorize(context.Background(), "work", true)
	if err != nil || !on.Authorized || on.State != NotInstalled {
		t.Fatalf("Authorize = %+v, %v; want work on and saying RepoGo isn't installed", on, err)
	}
	f.answers["work"] = answer{stdout: `{"host_id": "H-WORK"}`}
	if got := svc.List(context.Background(), true).Hosts[1]; got.State != Ready || !got.Authorized {
		t.Fatalf("once installed %+v, want ready and on", got)
	}
	if _, err := svc.Authorize(context.Background(), "nope", true); !errors.Is(err, errkind.ErrNotFound) {
		t.Fatalf("unknown: %v, want not found", err)
	}
}

// A host that went offline can still be switched off, and then isn't dialed.
func TestAnOfflineHostCanBeSwitchedOff(t *testing.T) {
	svc, f := onStudio(t, &sender{})
	f.answers["studio"] = answer{stderr: "ssh: connect to host studio: Operation timed out", code: 255}
	// Still shown as on, so the phone can offer its switch; still no calls.
	if got := svc.List(context.Background(), true).Hosts[0]; got.State != Offline || !got.Authorized {
		t.Fatalf("%+v, want studio offline and still switched on", got)
	}
	if _, err := svc.Call(context.Background(), "studio", Request{}, true); !errors.Is(err, errkind.ErrUnavailable) {
		t.Fatalf("a call to an offline host that's on: %v, want unavailable", err)
	}
	if off, err := svc.Authorize(context.Background(), "studio", false); err != nil || off.Authorized {
		t.Fatalf("Authorize off = %+v, %v", off, err)
	}
	f.ran = nil
	if got := svc.List(context.Background(), true).Hosts[0]; got.State != Off || got.Authorized || len(f.ran) != 0 {
		t.Fatalf("%+v after %d SSH calls, want off and not dialed", got, len(f.ran))
	}
}

// A switch flipped as the list is turned off doesn't leave a host on behind it.
func TestNothingIsSwitchedOnWhileTheListIsOff(t *testing.T) {
	svc, f := listedStudio(t, &sender{})
	if changed, _, err := svc.choices.setListing(false); err != nil || !changed {
		t.Fatal(changed, err)
	}
	// The look this host kept still names studio, as a phone's screen does.
	if _, err := svc.Authorize(context.Background(), "studio", true); !errors.Is(err, errkind.ErrDenied) {
		t.Fatalf("Authorize with the list off = %v, want denied", err)
	}
	if svc.choices.has("studio") || len(f.ran) != 0 {
		t.Fatalf("studio is on, or was dialed (%v), with the list off", f.ran)
	}
}

// A host whose id can't be saved isn't used: nothing would hold its next
// answer to this one.
func TestAHostWhoseIDCantBeSavedIsntUsed(t *testing.T) {
	dir := t.TempDir()
	svc, _, _ := studioAt(t, filepath.Join(dir, AuthorizedFile), "H1")
	if _, err := svc.SetListing(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// The switch is saved by hand, then the folder stops taking writes.
	if _, err := svc.choices.set("studio", true, svc.now()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	got := svc.List(context.Background(), true).Hosts[0]
	if got.State == Ready || !strings.Contains(got.Reason, "couldn't save") {
		t.Fatalf("%+v, want studio unusable, saying its id couldn't be saved", got)
	}
	if _, err := svc.Call(context.Background(), "studio", Request{}, true); !errors.Is(err, errkind.ErrUnavailable) {
		t.Fatalf("Call = %v, want unavailable", err)
	}
}
