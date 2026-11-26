package tunnelgateway

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RateLimits are fixed-window request counters per slug and per visitor IP.
// In memory, since the gateway runs as one machine.
type RateLimits struct {
	PerSlug int
	PerIP   int
	Window  time.Duration

	mu      sync.Mutex
	started time.Time
	counts  map[string]int
}

func NewRateLimits(perSlug, perIP int, window time.Duration) *RateLimits {
	return &RateLimits{PerSlug: perSlug, PerIP: perIP, Window: window, counts: map[string]int{}}
}

// AllowIP counts a request from ip, before its tunnel is looked up, so a
// flood of made-up names is refused before it costs a lookup.
func (l *RateLimits) AllowIP(ip string, now time.Time) bool {
	return l.allow("i:"+ip, l.PerIP, now)
}

// AllowSlug counts a request for a tunnel that exists.
func (l *RateLimits) AllowSlug(slug string, now time.Time) bool {
	return l.allow("s:"+slug, l.PerSlug, now)
}

// allow counts only requests it lets through, so a refused visitor can't keep
// a link at its limit for everyone else.
func (l *RateLimits) allow(key string, limit int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.started) >= l.Window {
		l.started, l.counts = now, map[string]int{}
	}
	if l.counts[key] >= limit {
		return false
	}
	l.counts[key]++
	return true
}

// clientIP is the visitor's address as Fly's edge saw it, else the peer.
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("Fly-Client-IP")); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
