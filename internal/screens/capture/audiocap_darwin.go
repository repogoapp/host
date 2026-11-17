//go:build darwin && cgo

package capture

/*
#cgo LDFLAGS: -framework CoreAudio -framework AudioToolbox
#include "audiocap_darwin.h"
*/
import "C"

import (
	"errors"
	"os"
)

// RunAudio writes this Mac's sound to out as AAC frames, as the capture
// children write video, until the process ends. mute silences the Mac's
// own speakers while it runs. Needs macOS 14.2 and the Mac's permission to
// record system audio, which macOS asks for the first time. exclude, when
// above 0, is a process whose sound is left out.
func RunAudio(out *os.File, mute bool, exclude int) error {
	m := 0
	if mute {
		m = 1
	}
	var why [256]C.char
	if C.audiocap_run(C.int(out.Fd()), C.int(m), C.int(exclude), &why[0], C.int(len(why))) != 0 {
		return errors.New(C.GoString(&why[0]))
	}
	return nil
}
