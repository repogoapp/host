//go:build !darwin

package projectwatch

// watchEvents has no tree watcher off macOS; inotify needs a watch per
// directory, so the fingerprint and the timer find changes instead.
func watchEvents([]string, func(string, bool)) (func(), bool) { return nil, false }
