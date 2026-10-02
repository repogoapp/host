// Package par is the one fan-out loop: a goroutine per input, results in
// input order, and an optional cap on how many run at once.
package par

import "sync"

// Map calls fn on every item concurrently and returns the results in order.
// limit caps the goroutines running at once; zero means all at once, which
// suits a few probes and not a batch of subprocesses.
func Map[T, R any](items []T, limit int, fn func(T) R) []R {
	out := make([]R, len(items))
	var slots chan struct{}
	if limit > 0 {
		slots = make(chan struct{}, limit)
	}
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if slots != nil {
				slots <- struct{}{}
				defer func() { <-slots }()
			}
			out[i] = fn(item)
		}()
	}
	wg.Wait()
	return out
}
