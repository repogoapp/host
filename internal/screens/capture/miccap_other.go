//go:build !(darwin && cgo)

package capture

import (
	"errors"
	"os"
)

const MicDevicePrefix = "app.repogo.iphone-mic"

// RunMic needs macOS.
func RunMic(*os.File, *os.File, string, string) error {
	return errors.New("the phone's mic needs macOS")
}

// RemoveMicDevices has nothing to remove off macOS.
func RemoveMicDevices() {}
