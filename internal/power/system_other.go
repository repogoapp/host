//go:build !darwin || !cgo

package power

// newSystem has nothing to hold off macOS; the Keeper does nothing.
func newSystem() system { return nil }
