//go:build darwin

package screens

import "golang.org/x/sys/unix"

// uptimeUS is now on the clock capture stamps frames with
// (CLOCK_UPTIME_RAW, simcap's nowNs), in microseconds; 0 if unreadable.
func uptimeUS() int64 {
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_UPTIME_RAW, &ts) != nil {
		return 0
	}
	return ts.Nano() / 1000
}
