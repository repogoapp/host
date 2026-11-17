package desktop

import "sync"

// Changes tells of each change the Mac reports, until stop is called: an
// app coming to the front, launching or quitting, the space changing, and
// in the front app a window opening, closing, focusing, minimizing,
// resizing or retitling. The receiver reads the desktop again; a burst of
// changes is one receive. The window changes need Accessibility; the rest
// don't.
func Changes() (changes <-chan struct{}, stop func()) {
	startWatch.Do(watch)
	c := make(chan struct{}, 1)
	watchersMu.Lock()
	watchers[c] = true
	watchersMu.Unlock()
	return c, func() {
		watchersMu.Lock()
		delete(watchers, c)
		watchersMu.Unlock()
	}
}

var (
	startWatch sync.Once
	watchersMu sync.Mutex
	watchers   = map[chan struct{}]bool{}
)

// changed tells every receiver, without waiting on any.
func changed() {
	watchersMu.Lock()
	defer watchersMu.Unlock()
	for c := range watchers {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}
