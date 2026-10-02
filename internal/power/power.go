// Package power keeps the Mac awake while the host has a paired device to serve:
// an idle-sleep assertion always, and through a closed lid once the user has run
// `repogo power enable`. Plan 306 has the research and the safety rules. The
// same tick reads the battery, which the host reports to its phones.
package power

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/syscmd"
)

func (Battery) Method() string { return "host.power" }

// SudoersPath is the rule `repogo power enable` installs; the lid hold needs it.
const SudoersPath = "/etc/sudoers.d/repogo"

const (
	pmset    = "/usr/bin/pmset"
	interval = 30 * time.Second
	// Below this on battery the lid hold lets the Mac sleep rather than drain it.
	lowBattery = 20
)

// system is the machine: system_darwin.go, or a fake in tests.
type system interface {
	holdIdle(on bool) error
	lidAllowed() bool
	// lidDisabled reports the kernel SleepDisabled flag, whoever set it.
	lidDisabled(ctx context.Context) (bool, error)
	setLid(ctx context.Context, on bool) error
	battery(ctx context.Context) (Battery, error)
	hot() bool
}

// Battery is the Mac's power as its phones show it. A desktop Mac has no
// battery and is always plugged in.
type Battery struct {
	Present   bool `json:"present"`
	Percent   int  `json:"percent"`
	Charging  bool `json:"charging"`
	PluggedIn bool `json:"plugged_in"`
}

// low reports whether the battery alone is running the Mac, below percent.
func (b Battery) low(percent int) bool { return b.Present && !b.PluggedIn && b.Percent < percent }

type Keeper struct {
	log    *slog.Logger
	sys    system
	wanted func() bool
	marker string

	// hold serializes the holds, which wait on pmset and sudo; state below
	// it is its. mu guards only the reading, so Battery never waits on them.
	hold    sync.Mutex
	idle    bool
	lid     bool
	closed  bool
	lastErr string

	mu sync.Mutex
	// The last battery reading, and whether there has been one.
	batt     Battery
	hasBatt  bool
	onChange func(Battery)
}

// New returns a Keeper that holds while wanted reports true. onBattery is
// called, off the lock, with each reading that differs from the one before it.
func New(log *slog.Logger, wanted func() bool, onBattery func(Battery)) (*Keeper, error) {
	marker, err := markerPath()
	if err != nil {
		return nil, err
	}
	return newKeeper(log, newSystem(), wanted, onBattery, marker), nil
}

func newKeeper(log *slog.Logger, sys system, wanted func() bool, onBattery func(Battery), marker string) *Keeper {
	k := &Keeper{log: log, sys: sys, wanted: wanted, onChange: onBattery, marker: marker}
	// A marker left by a host that died holding the flag makes the flag ours again.
	_, err := os.Stat(marker)
	k.lid = err == nil
	return k
}

// Battery is the last reading; false until the first tick, and off macOS.
func (k *Keeper) Battery() (Battery, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.batt, k.hasBatt
}

// Run reconciles the holds until ctx ends; Release clears them.
func (k *Keeper) Run(ctx context.Context) {
	if k.sys == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		k.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Release drops both holds for good; the host calls it on shutdown.
func (k *Keeper) Release() {
	if k.sys == nil {
		return
	}
	k.hold.Lock()
	defer k.hold.Unlock()
	k.closed = true
	k.setIdle(false)
	if k.lid {
		// Shutdown's own: the host's context has already ended.
		k.setLid(context.Background(), false)
	}
}

func (k *Keeper) reconcile(ctx context.Context) {
	if b, changed := k.holdAll(ctx); changed {
		k.onChange(b)
	}
}

// holdAll sets both holds and reports the battery if it moved since last time.
func (k *Keeper) holdAll(ctx context.Context) (Battery, bool) {
	k.hold.Lock()
	defer k.hold.Unlock()
	if k.closed {
		return Battery{}, false
	}
	want := k.wanted()
	k.setIdle(want)

	hot := k.sys.hot()
	// An unreadable battery is not a low one: the lid hold stands and the
	// phones keep the last reading.
	b, err := k.sys.battery(ctx)
	if err != nil {
		k.fail("read battery failed", err)
	}
	lid := want && k.sys.lidAllowed() && !hot && (err != nil || !b.low(lowBattery))
	if lid != k.lid && k.setLid(ctx, lid) {
		k.log.Info("lid sleep hold", "on", lid, "paired", want, "hot", hot,
			"battery", b.Percent, "plugged_in", b.PluggedIn)
	}
	if err != nil {
		return Battery{}, false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.hasBatt && b == k.batt {
		return Battery{}, false
	}
	k.batt, k.hasBatt = b, true
	return b, true
}

func (k *Keeper) setIdle(on bool) {
	if on == k.idle {
		return
	}
	if err := k.sys.holdIdle(on); err != nil {
		k.fail("idle sleep hold failed", err)
		return
	}
	k.idle = on
	k.log.Info("idle sleep hold", "on", on)
}

// setLid reports whether the flag changed hands.
func (k *Keeper) setLid(ctx context.Context, on bool) bool {
	if on {
		// A SleepDisabled the user set is theirs: the host neither sets nor clears it.
		set, err := k.sys.lidDisabled(ctx)
		if err != nil {
			k.fail("read SleepDisabled failed", err)
			return false
		}
		if set {
			return false
		}
		if err := os.WriteFile(k.marker, nil, 0o600); err != nil {
			k.fail("lid hold marker failed", err)
			return false
		}
		if err := k.sys.setLid(ctx, true); err != nil {
			_ = os.Remove(k.marker)
			k.fail("lid sleep hold failed", err)
			return false
		}
	} else {
		// Without the rule `repogo power disable` has already cleared the flag.
		if err := k.sys.setLid(ctx, false); err != nil && k.sys.lidAllowed() {
			k.fail("lid sleep release failed", err)
			return false
		}
		if err := os.Remove(k.marker); err != nil && !os.IsNotExist(err) {
			k.fail("lid hold marker failed", err)
		}
	}
	k.lid = on
	k.lastErr = ""
	return true
}

// fail logs an error once, not on every tick it repeats.
func (k *Keeper) fail(msg string, err error) {
	if s := msg + err.Error(); s != k.lastErr {
		k.lastErr = s
		k.log.Warn(msg, "err", err)
	}
}

func markerPath() (string, error) { return apphome.MkdirAll("lid-hold") }

func pmsetArgs(on bool) []string {
	v := "0"
	if on {
		v = "1"
	}
	return []string{"-a", "disablesleep", v}
}

var username = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// SudoersRule lets user run exactly the two pmset commands the lid hold needs.
func SudoersRule(user string) (string, error) {
	if !username.MatchString(user) {
		return "", fmt.Errorf("user name %q cannot go in a sudoers rule", user)
	}
	command := func(on bool) string { return pmset + " " + strings.Join(pmsetArgs(on), " ") }
	return fmt.Sprintf("# repogo power enable: lets the RepoGo host keep this Mac awake with the lid closed.\n"+
		"%s ALL=(root) NOPASSWD: %s, %s\n", user, command(false), command(true)), nil
}

// A battery line of `pmset -g batt`, its level and state:
// " -InternalBattery-0 (id=4653155)	85%; charging; 1:02 remaining present: true".
var batteryLine = regexp.MustCompile(`InternalBattery[^\t]*\t\s*(\d+)%;\s*([^;]+);`)

// parseBattery reads `pmset -g batt`. A Mac with no internal battery prints
// only the source line, and is plugged in.
func parseBattery(out string) Battery {
	b := Battery{PluggedIn: !strings.Contains(out, "'Battery Power'")}
	m := batteryLine.FindStringSubmatch(out)
	if m == nil {
		return b
	}
	b.Present = true
	fmt.Sscan(m[1], &b.Percent)
	// "discharging", "AC attached; not charging" and "charged" are all not charging.
	state := strings.TrimSpace(m[2])
	b.Charging = state == "charging" || state == "finishing charge"
	return b
}

// parseSleepDisabled reads the SleepDisabled line of `pmset -g`.
func parseSleepDisabled(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "SleepDisabled" {
			return f[1] == "1"
		}
	}
	return false
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	return syscmd.Output(ctx, 10*time.Second, name, args...)
}
