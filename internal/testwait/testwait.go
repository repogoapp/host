// Package testwait polls a condition in tests whose code under test offers no
// hook to wait on, such as a child process or a socket on another goroutine.
package testwait

import (
	"testing"
	"time"
)

// Timeout is generous because slow CI machines, not the code, set the pace.
const Timeout = 10 * time.Second

// For polls cond until it holds, failing t with what after Timeout.
func For(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(Timeout)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}
