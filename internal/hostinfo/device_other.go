//go:build !darwin && !linux

package hostinfo

import "runtime"

func readDevice() Device {
	os := runtime.GOOS
	if os != "windows" {
		os = "unknown"
	}
	return Device{OS: os, FormFactor: FormDesktop}
}
