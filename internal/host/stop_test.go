package host_test

import (
	"sync"
	"testing"
	"time"

	"github.com/repogo/host/internal/host"
)

// A worker stuck past the grace lets Close go on, so an update still restarts.
func TestWaitWithinGivesUpOnAStuckWorker(t *testing.T) {
	var wg sync.WaitGroup
	release := make(chan struct{})
	wg.Go(func() { <-release })

	if host.WaitWithin(&wg, 10*time.Millisecond) {
		t.Fatal("waited out a worker that never stopped")
	}
	close(release)
	if !host.WaitWithin(&wg, time.Minute) {
		t.Fatal("gave up on a worker that stopped")
	}
}
