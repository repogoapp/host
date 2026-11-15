//go:build darwin && cgo

package capture

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework CoreGraphics -framework CoreMedia -framework CoreVideo -framework VideoToolbox -framework IOSurface -framework IOKit -framework Carbon -framework Accelerate
#include <stdlib.h>
#include "simcap_darwin.h"
*/
import "C"

import (
	"errors"
	"os"
	"unsafe"
)

func start(developerDir, udid string, universal bool, out *os.File) error {
	cDir, cUDID := C.CString(developerDir), C.CString(udid)
	defer C.free(unsafe.Pointer(cDir))
	defer C.free(unsafe.Pointer(cUDID))
	u := 0
	if universal {
		u = 1
	}
	var why [256]C.char
	if C.simcap_start(cDir, cUDID, C.int(u), C.int(out.Fd()), &why[0], C.int(len(why))) != 0 {
		return errors.New(C.GoString(&why[0]))
	}
	return nil
}

func touch(phase int, x, y float64) { C.simcap_touch(C.int(phase), C.double(x), C.double(y)) }

func button(source, holdMS int) { C.simcap_button(C.int(source), C.int(holdMS)) }

func hid(page, usage int) { C.simcap_hid(C.int(page), C.int(usage)) }

func key(usage int, down bool) {
	d := 0
	if down {
		d = 1
	}
	C.simcap_key(C.int(usage), C.int(d))
}

func rotate(orientation int) bool { return C.simcap_rotate(C.int(orientation)) == 0 }

func memoryWarning() { C.simcap_memory_warning() }

func onscreenKeyboard(on bool) bool {
	v := 0
	if on {
		v = 1
	}
	return C.simcap_onscreen_keyboard(C.int(v)) == 0
}

func fold(pose string) bool {
	p := C.CString(pose)
	defer C.free(unsafe.Pointer(p))
	return C.simcap_fold(p) == 0
}

func failed(why string) {
	w := C.CString(why)
	defer C.free(unsafe.Pointer(w))
	C.simcap_failed(w)
}

func caDebug(name string, on bool) {
	n := C.CString(name)
	defer C.free(unsafe.Pointer(n))
	enabled := 0
	if on {
		enabled = 1
	}
	C.simcap_ca_debug(n, C.int(enabled))
}

func stop() { C.simcap_stop() }
