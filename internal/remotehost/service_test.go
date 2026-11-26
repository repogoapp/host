package remotehost

import (
	"context"
	"testing"
	"time"
)

// Until the user asks for the list, ~/.ssh/config isn't opened at all.
func TestTheSSHFileIsntReadUntilTheUserAsksForTheList(t *testing.T) {
	f := &fake{config: "Host studio\n", answers: map[string]answer{"studio": {stdout: `{"host_id": "H1"}`}}}
	svc := NewService(f.deps(""))
	for _, fresh := range []bool{false, true} {
		if got := svc.List(context.Background(), fresh); got.Listing || len(got.Hosts) != 0 {
			t.Fatalf("List = %+v, want nothing listed", got)
		}
	}
	if _, err := svc.Authorize(context.Background(), "studio", true); err == nil {
		t.Fatal("switched on a host that isn't listed")
	}
	if f.read != 0 || len(f.ran) != 0 {
		t.Fatalf("read the file %d times and dialed %v before the user asked", f.read, f.ran)
	}

	got, err := svc.SetListing(context.Background(), true)
	if err != nil || !got.Listing || len(got.Hosts) != 1 || got.Hosts[0].State != Off || len(f.ran) != 0 {
		t.Fatalf("SetListing = %+v, %v after dialing %v; want studio listed, off and not dialed", got, err, f.ran)
	}
}

// Turning the list off switches every host off, hangs up, and stops reading.
func TestTurningTheListOffSwitchesEveryHostOff(t *testing.T) {
	svc, f := onStudio(t, &sender{})
	got, err := svc.SetListing(context.Background(), false)
	if err != nil || got.Listing || len(got.Hosts) != 0 {
		t.Fatalf("SetListing off = %+v, %v; want nothing listed", got, err)
	}
	if len(f.closed) != 1 || f.closed[0] != "studio" {
		t.Fatalf("closed %v, want studio's login ended", f.closed)
	}
	read := f.read
	svc.List(context.Background(), true)
	if f.read != read {
		t.Fatal("read the file with the list off")
	}
	if on, _ := svc.SetListing(context.Background(), true); on.Hosts[0].Authorized {
		t.Fatalf("%+v: studio is still on after the list was turned off and on", on.Hosts[0])
	}
}

// A look is reused for two minutes, then runs again; fresh always runs.
func TestListReusesARecentLook(t *testing.T) {
	svc, f := listedStudio(t, &sender{})
	now := time.UnixMilli(1_000_000)
	svc.now = func() time.Time { return now }
	if _, err := svc.Authorize(context.Background(), "studio", true); err != nil {
		t.Fatal(err)
	}
	f.ran = nil
	f.answers["studio"] = answer{stderr: "ssh: connect to host studio: timed out", code: 255}

	now = now.Add(Reuse - time.Second)
	if got := svc.List(context.Background(), false); got.Hosts[0].State != Ready || len(f.ran) != 0 {
		t.Fatalf("List under Reuse = %+v after %d SSH calls, want the kept look", got, len(f.ran))
	}
	if got := svc.List(context.Background(), true); got.Hosts[0].State != Offline || len(f.ran) != 1 {
		t.Fatalf("fresh List = %+v after %d SSH calls, want a new look", got, len(f.ran))
	}
	now = now.Add(Reuse)
	if svc.List(context.Background(), false); len(f.ran) != 2 {
		t.Fatalf("List past Reuse made %d SSH calls, want another look", len(f.ran))
	}
}

// A look cut short keeps the last full one rather than calling every host offline.
func TestALookCutShortKeepsTheLastFullOne(t *testing.T) {
	svc, _ := onStudio(t, &sender{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := svc.List(ctx, true); len(got.Hosts) != 2 || got.Hosts[0].State != Ready {
		t.Fatalf("cancelled List = %+v, want the last full look", got)
	}
}
