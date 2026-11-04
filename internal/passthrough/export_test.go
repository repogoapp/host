package passthrough

import "github.com/repogoapp/host/internal/device"

// open is how many connections the relay holds for an address.
func (r *Relay) open(ip string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.perIP[ip]
}

// pinged is how many pings the relay has written on a host's current link.
func (r *Relay) pinged(id device.ID) int {
	r.mu.Lock()
	l := r.links[id]
	r.mu.Unlock()
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pings
}

// waitingOn is how many phone streams wait on a host's dial-back.
func (r *Relay) waitingOn(id device.ID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.waiting[id]
}
