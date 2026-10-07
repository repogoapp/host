package hostlink

import "time"

const (
	MaxStrangers = maxStrangers
	StrangerTTL  = strangerTTL
)

// PeerCount is how many phones the attached connection holds state for.
func (l *Link) PeerCount() int {
	l.mu.RLock()
	c := l.current
	l.mu.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.peers)
}

// SetNow replaces the clock the stranger sweep reads.
func (l *Link) SetNow(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}
