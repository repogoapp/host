package wsserver

import (
	"testing"
	"time"
)

// ShortenTimeouts sets the hello and pairing deadlines and the cap on
// unproven connections for one test.
func ShortenTimeouts(t *testing.T, hello, pair time.Duration, unproven int32) {
	oldHello, oldPair, oldCap := remoteHelloTimeout, pairTimeout, maxUnproven
	remoteHelloTimeout, pairTimeout, maxUnproven = hello, pair, unproven
	t.Cleanup(func() { remoteHelloTimeout, pairTimeout, maxUnproven = oldHello, oldPair, oldCap })
}

// CapOpen sets how many sockets one remote listener holds, for one test.
func CapOpen(t *testing.T, n int) {
	old := maxOpen
	maxOpen = n
	t.Cleanup(func() { maxOpen = old })
}
