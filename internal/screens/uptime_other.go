//go:build !darwin

package screens

// uptimeUS is 0 off a Mac: no capture stamps frames on its clock.
func uptimeUS() int64 { return 0 }
