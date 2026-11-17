//go:build darwin && cgo

package capture

/*
#cgo LDFLAGS: -framework CoreAudio -framework AudioToolbox
#include <stdlib.h>
#include "miccap_darwin.h"
*/
import "C"

import (
	"errors"
	"os"
	"unsafe"
)

// MicDevicePrefix starts every phone mic device's ID.
const MicDevicePrefix = "app.repogo.iphone-mic"

// RunMic makes a phone's microphone an input on this Mac, the device uid
// named name, playing the PCM read from in until it closes. Needs macOS 14.2.
func RunMic(in, out *os.File, uid, name string) error {
	cUID, cName := C.CString(uid), C.CString(name)
	defer C.free(unsafe.Pointer(cUID))
	defer C.free(unsafe.Pointer(cName))
	var why [256]C.char
	if C.miccap_run(C.int(in.Fd()), C.int(out.Fd()), cUID, cName, &why[0], C.int(len(why))) != 0 {
		return errors.New(C.GoString(&why[0]))
	}
	return nil
}

// RemoveMicDevices removes every phone mic device: run when the host
// starts, before any mic of its own, for what a killed run left.
func RemoveMicDevices() {
	prefix := C.CString(MicDevicePrefix)
	defer C.free(unsafe.Pointer(prefix))
	C.miccap_remove(prefix)
}
