//go:build !darwin && !windows

package screens

// uptimeUS is 0 off a Mac or Windows: no capture stamps frames on its clock.
func uptimeUS() int64 { return 0 }
