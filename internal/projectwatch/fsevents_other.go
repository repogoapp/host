//go:build !darwin && !linux && !windows

package projectwatch

// watchEvents has no tree watcher here, so the fingerprint and the timer find
// changes instead.
func watchEvents([]string, func(string) bool, func(string, bool), func()) (func(), bool) {
	return nil, false
}
