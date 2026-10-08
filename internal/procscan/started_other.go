//go:build !darwin && !linux

package procscan

// Started is unknown here, so no process outlives a restart as a run.
func Started(int) (int64, bool) { return 0, false }
