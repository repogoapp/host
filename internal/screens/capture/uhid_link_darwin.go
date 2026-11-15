//go:build darwin && cgo && simcap_uhid

package capture

/*
#cgo arm64 LDFLAGS: ${SRCDIR}/uhid/build/arm64/libsimcapuhid.a
#cgo amd64 LDFLAGS: ${SRCDIR}/uhid/build/x86_64/libsimcapuhid.a
#cgo LDFLAGS: -L/usr/lib/swift -Wl,-rpath,/usr/lib/swift
*/
import "C"

// UniversalHID reports whether this build links the Swift bridge that
// iOS 27 simulators' touches and keys go through.
const UniversalHID = true
