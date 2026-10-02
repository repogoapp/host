package power

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeSystem struct {
	idle, flag, allowed, isHot, onBattery bool
	percent                               int
	lidCalls                              int
	failSet                               bool
	battErr                               error
}

func (f *fakeSystem) holdIdle(on bool) error                    { f.idle = on; return nil }
func (f *fakeSystem) lidAllowed() bool                          { return f.allowed }
func (f *fakeSystem) lidDisabled(context.Context) (bool, error) { return f.flag, nil }
func (f *fakeSystem) hot() bool                                 { return f.isHot }
func (f *fakeSystem) setLid(_ context.Context, on bool) error {
	f.lidCalls++
	if f.failSet {
		return errors.New("sudo: a password is required")
	}
	f.flag = on
	return nil
}

func (f *fakeSystem) battery(context.Context) (Battery, error) {
	return Battery{Present: true, Percent: f.percent, PluggedIn: !f.onBattery}, f.battErr
}

func newTestKeeper(t *testing.T, sys *fakeSystem, paired *bool) (*Keeper, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "lid-hold")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newKeeper(log, sys, func() bool { return *paired }, func(Battery) {}, marker), marker
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestHoldsWhilePairedAndReleasesOnShutdown(t *testing.T) {
	sys := &fakeSystem{allowed: true, percent: 80}
	paired := true
	k, marker := newTestKeeper(t, sys, &paired)

	k.reconcile(t.Context())
	if !sys.idle || !sys.flag || !exists(marker) {
		t.Fatalf("paired: idle=%v flag=%v marker=%v, want all held", sys.idle, sys.flag, exists(marker))
	}
	k.Release()
	if sys.idle || sys.flag || exists(marker) {
		t.Fatalf("released: idle=%v flag=%v marker=%v, want none", sys.idle, sys.flag, exists(marker))
	}
	k.reconcile(t.Context())
	if sys.idle || sys.flag {
		t.Fatal("a released keeper held again")
	}
}

func TestNoPairedDeviceHoldsNothing(t *testing.T) {
	sys := &fakeSystem{allowed: true, percent: 80}
	paired := true
	k, marker := newTestKeeper(t, sys, &paired)
	k.reconcile(t.Context())
	paired = false
	k.reconcile(t.Context())
	if sys.idle || sys.flag || exists(marker) {
		t.Fatalf("unpaired: idle=%v flag=%v marker=%v, want none", sys.idle, sys.flag, exists(marker))
	}
}

func TestLidHoldNeedsTheRule(t *testing.T) {
	sys := &fakeSystem{percent: 80}
	paired := true
	k, _ := newTestKeeper(t, sys, &paired)
	k.reconcile(t.Context())
	if !sys.idle || sys.flag || sys.lidCalls != 0 {
		t.Fatalf("idle=%v flag=%v calls=%d, want idle only", sys.idle, sys.flag, sys.lidCalls)
	}
}

func TestHeatAndLowBatteryReleaseTheLid(t *testing.T) {
	for name, sys := range map[string]*fakeSystem{
		"hot":         {allowed: true, percent: 80, isHot: true},
		"low battery": {allowed: true, percent: 19, onBattery: true},
	} {
		t.Run(name, func(t *testing.T) {
			paired := true
			hot, onBattery := sys.isHot, sys.onBattery
			sys.isHot, sys.onBattery = false, false
			k, _ := newTestKeeper(t, sys, &paired)
			k.reconcile(t.Context())
			if !sys.flag {
				t.Fatal("lid not held while safe")
			}
			sys.isHot, sys.onBattery = hot, onBattery
			k.reconcile(t.Context())
			if sys.flag || !sys.idle {
				t.Fatalf("flag=%v idle=%v, want the lid released and idle kept", sys.flag, sys.idle)
			}
		})
	}
	sys := &fakeSystem{allowed: true, percent: 10}
	paired := true
	k, _ := newTestKeeper(t, sys, &paired)
	k.reconcile(t.Context())
	if !sys.flag {
		t.Fatal("a low battery on AC power released the lid")
	}
}

func TestUserSetFlagIsLeftAlone(t *testing.T) {
	sys := &fakeSystem{allowed: true, percent: 80, flag: true}
	paired := true
	k, marker := newTestKeeper(t, sys, &paired)
	k.reconcile(t.Context())
	k.Release()
	if !sys.flag || sys.lidCalls != 0 || exists(marker) {
		t.Fatalf("flag=%v calls=%d marker=%v, want the user's flag untouched", sys.flag, sys.lidCalls, exists(marker))
	}
}

func TestMarkerFromACrashedHostIsReclaimed(t *testing.T) {
	sys := &fakeSystem{allowed: true, percent: 80, flag: true}
	paired := false
	marker := filepath.Join(t.TempDir(), "lid-hold")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	k := newKeeper(slog.New(slog.NewTextHandler(io.Discard, nil)), sys, func() bool { return paired }, func(Battery) {}, marker)
	k.reconcile(t.Context())
	if sys.flag || exists(marker) {
		t.Fatalf("flag=%v marker=%v, want the stale hold cleared", sys.flag, exists(marker))
	}
}

func TestRemovedRuleForgetsTheHold(t *testing.T) {
	sys := &fakeSystem{allowed: true, percent: 80}
	paired := true
	k, marker := newTestKeeper(t, sys, &paired)
	k.reconcile(t.Context())
	// `repogo power disable` removed the rule and cleared the flag itself.
	sys.allowed, sys.failSet, sys.flag = false, true, false
	k.reconcile(t.Context())
	if k.lid || exists(marker) {
		t.Fatalf("lid=%v marker=%v, want the hold forgotten", k.lid, exists(marker))
	}
}

func TestFailedSetLeavesNoMarker(t *testing.T) {
	sys := &fakeSystem{allowed: true, percent: 80, failSet: true}
	paired := true
	k, marker := newTestKeeper(t, sys, &paired)
	k.reconcile(t.Context())
	if k.lid || exists(marker) {
		t.Fatalf("lid=%v marker=%v, want nothing held", k.lid, exists(marker))
	}
}

func TestParseBattery(t *testing.T) {
	line := func(state string) string {
		return " -InternalBattery-0 (id=4653155)\t" + state + " remaining present: true\n"
	}
	for _, tc := range []struct {
		out  string
		want Battery
	}{
		{"Now drawing from 'Battery Power'\n" + line("17%; discharging; 1:02"), Battery{Present: true, Percent: 17}},
		{"Now drawing from 'AC Power'\n" + line("85%; charging; 0:40"), Battery{Present: true, Percent: 85, Charging: true, PluggedIn: true}},
		{"Now drawing from 'AC Power'\n" + line("98%; finishing charge; 0:05"), Battery{Present: true, Percent: 98, Charging: true, PluggedIn: true}},
		{"Now drawing from 'AC Power'\n" + line("80%; AC attached; not charging"), Battery{Present: true, Percent: 80, PluggedIn: true}},
		{"Now drawing from 'AC Power'\n" + line("100%; charged; 0:00"), Battery{Present: true, Percent: 100, PluggedIn: true}},
		// A desktop Mac: no battery, and never on one.
		{"Now drawing from 'AC Power'\n", Battery{PluggedIn: true}},
	} {
		if got := parseBattery(tc.out); got != tc.want {
			t.Errorf("parseBattery(%q) = %+v; want %+v", tc.out, got, tc.want)
		}
	}
}

func TestBatteryIsReportedWhenItMoves(t *testing.T) {
	sys := &fakeSystem{percent: 80}
	paired := false
	k, _ := newTestKeeper(t, sys, &paired)
	var seen []Battery
	k.onChange = func(b Battery) { seen = append(seen, b) }

	if _, ok := k.Battery(); ok {
		t.Fatal("a reading before the first tick")
	}
	k.reconcile(t.Context())
	k.reconcile(t.Context())
	sys.onBattery = true
	k.reconcile(t.Context())
	want := []Battery{{Present: true, Percent: 80, PluggedIn: true}, {Present: true, Percent: 80}}
	if len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("reported %+v, want %+v", seen, want)
	}
	if b, ok := k.Battery(); !ok || b != want[1] {
		t.Fatalf("Battery() = %+v, %v; want %+v", b, ok, want[1])
	}
}

func TestUnreadableBatteryKeepsTheLidAndTheLastReading(t *testing.T) {
	sys := &fakeSystem{allowed: true, percent: 50}
	paired := true
	k, _ := newTestKeeper(t, sys, &paired)
	calls := 0
	k.onChange = func(Battery) { calls++ }
	k.reconcile(t.Context())
	sys.percent, sys.onBattery, sys.battErr = 5, true, errors.New("pmset: exit status 1")
	k.reconcile(t.Context())
	if !sys.flag {
		t.Fatal("an unreadable battery released the lid")
	}
	if b, _ := k.Battery(); calls != 1 || b.Percent != 50 {
		t.Fatalf("calls=%d last=%+v, want the one good reading kept", calls, b)
	}
}

func TestParseSleepDisabled(t *testing.T) {
	out := "System-wide power settings:\n SleepDisabled\t\t1\nCurrently in use:\n standby              1\n"
	if !parseSleepDisabled(out) {
		t.Fatal("SleepDisabled 1 not read")
	}
	if parseSleepDisabled(strings.Replace(out, "\t\t1", "\t\t0", 1)) {
		t.Fatal("SleepDisabled 0 read as set")
	}
}

func TestSudoersRule(t *testing.T) {
	rule, err := SudoersRule("alice")
	if err != nil {
		t.Fatal(err)
	}
	want := "alice ALL=(root) NOPASSWD: /usr/bin/pmset -a disablesleep 0, /usr/bin/pmset -a disablesleep 1\n"
	if !strings.HasSuffix(rule, want) {
		t.Fatalf("rule = %q, want suffix %q", rule, want)
	}
	for _, bad := range []string{"", "a b", "root,ALL", "x\nALL ALL=(ALL) ALL"} {
		if _, err := SudoersRule(bad); err == nil {
			t.Errorf("SudoersRule(%q) accepted", bad)
		}
	}
}

// slowBattery is a pmset that hangs until released.
type slowBattery struct {
	*fakeSystem
	entered, release chan struct{}
}

func (s slowBattery) battery(ctx context.Context) (Battery, error) {
	close(s.entered)
	<-s.release
	return s.fakeSystem.battery(ctx)
}

// host.status reads the battery; it must not wait out a pmset that hangs.
func TestBatteryDoesNotWaitOnAHoldInProgress(t *testing.T) {
	sys := slowBattery{&fakeSystem{allowed: true, percent: 80}, make(chan struct{}), make(chan struct{})}
	marker := filepath.Join(t.TempDir(), "lid-hold")
	k := newKeeper(slog.New(slog.NewTextHandler(io.Discard, nil)), sys, func() bool { return true }, func(Battery) {}, marker)
	done := make(chan struct{})
	go func() { k.reconcile(t.Context()); close(done) }()
	<-sys.entered
	if _, ok := k.Battery(); ok {
		t.Fatal("a reading before the first tick finished")
	}
	close(sys.release)
	<-done
	if b, ok := k.Battery(); !ok || b.Percent != 80 {
		t.Fatalf("Battery = %+v, %v after the tick", b, ok)
	}
}
