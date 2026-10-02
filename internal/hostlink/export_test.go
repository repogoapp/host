package hostlink

// PeerCount is how many phones the attached connection holds state for.
func (l *Link) PeerCount() int {
	l.mu.RLock()
	c := l.current
	l.mu.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.peers)
}
