//go:build darwin && cgo

package power

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework IOKit -framework Foundation
#import <Foundation/Foundation.h>
#import <IOKit/pwr_mgt/IOPMLib.h>

static IOReturn repogo_hold_idle(IOPMAssertionID *id) {
	return IOPMAssertionCreateWithName(kIOPMAssertionTypePreventUserIdleSystemSleep,
		kIOPMAssertionLevelOn, CFSTR("RepoGo host is serving paired devices"), id);
}

static int repogo_thermal_state(void) {
	return (int)[[NSProcessInfo processInfo] thermalState];
}
*/
import "C"

import (
	"context"
	"fmt"
)

// NSProcessInfoThermalStateSerious: a Mac this hot must be let sleep.
const thermalSerious = 2

// darwin holds the idle assertion itself; powerd drops it if the host exits.
type darwin struct{ assertion C.IOPMAssertionID }

func newSystem() system { return &darwin{} }

func (d *darwin) holdIdle(on bool) error {
	if on {
		if r := C.repogo_hold_idle(&d.assertion); r != 0 {
			return fmt.Errorf("IOPMAssertionCreateWithName: %#x", uint32(r))
		}
		return nil
	}
	if r := C.IOPMAssertionRelease(d.assertion); r != 0 {
		return fmt.Errorf("IOPMAssertionRelease: %#x", uint32(r))
	}
	return nil
}

func (d *darwin) lidAllowed() bool { return Enabled() }

func (d *darwin) lidDisabled(ctx context.Context) (bool, error) {
	out, err := output(ctx, pmset, "-g")
	return parseSleepDisabled(out), err
}

func (d *darwin) setLid(ctx context.Context, on bool) error {
	_, err := output(ctx, "/usr/bin/sudo", append([]string{"-n", pmset}, pmsetArgs(on)...)...)
	return err
}

func (d *darwin) battery(ctx context.Context) (Battery, error) {
	out, err := output(ctx, pmset, "-g", "batt")
	if err != nil {
		return Battery{}, err
	}
	return parseBattery(out), nil
}

func (d *darwin) hot() bool { return C.repogo_thermal_state() >= thermalSerious }
