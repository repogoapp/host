//go:build !(darwin && cgo)

package capture

// UniversalHID reports whether this build links the Swift bridge that
// iOS 27 simulators' touches and keys go through; only macOS builds can.
const UniversalHID = false
