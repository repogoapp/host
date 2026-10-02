package par

import (
	"sync/atomic"
	"testing"
)

func TestMapKeepsInputOrder(t *testing.T) {
	got := Map([]int{3, 1, 2}, 0, func(n int) int { return n * 10 })
	if len(got) != 3 || got[0] != 30 || got[1] != 10 || got[2] != 20 {
		t.Fatalf("Map = %v", got)
	}
}

// limit caps how many calls run at once: each call waits until limit of them
// are in flight, so a cap that is not honoured shows as more than limit.
func TestMapHonoursTheLimit(t *testing.T) {
	const limit = 2
	var running, peak atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	done := make(chan []int)
	go func() {
		done <- Map(make([]int, 8), limit, func(int) int {
			n := running.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			started <- struct{}{}
			<-release
			running.Add(-1)
			return 1
		})
	}()
	for range limit {
		<-started
	}
	close(release)
	if out := <-done; len(out) != 8 {
		t.Fatalf("Map returned %d results", len(out))
	}
	if p := peak.Load(); p > limit {
		t.Fatalf("%d ran at once, limit %d", p, limit)
	}
}
