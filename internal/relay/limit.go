package relay

import (
	"context"
	"net"
	"net/http"
	"time"
)

// Every frame a connection sends is paced, waiting rather than dropping; the
// frame rate only stops a runaway sender.
const (
	messagesPerSecond = 2000
	messageBurst      = 10000

	bytesPerSecond = 8 << 20
	// At least one maximum-size message, so the largest frame is never refused.
	byteBurst = 4 * MaxFrameBytes

	// defaultConnsPerIP bounds concurrent sockets from one address, handshakes
	// included, so one machine cannot fill the relay. Room for an office or
	// carrier NAT with several phones and hosts behind one address.
	defaultConnsPerIP = 16

	// sitesPerIP sizes an IPv6 /48's cap in subscribers' worth, since one
	// cheap server is often handed a whole /48 of /64s.
	sitesPerIP = 4

	// Reconnects spend a budget even after their concurrent socket slot is freed.
	connectionAttemptsPerMinute = 240
	maxAttemptIPs               = 4096
)

type limits struct {
	messages, messageBurst float64
	bytes, byteBurst       float64
}

var defaultLimits = limits{
	messages: messagesPerSecond, messageBurst: messageBurst,
	bytes: bytesPerSecond, byteBurst: byteBurst,
}

// bucket is a token bucket that waits instead of refusing. It runs in one
// connection's read loop, so it needs no lock.
type bucket struct {
	rate, burst, tokens float64
	last                time.Time
}

func newBucket(rate, burst float64) *bucket {
	return &bucket{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

// wait takes n tokens, sleeping off any debt. n above the burst counts as the
// burst, so one large frame waits for a full bucket, never forever.
func (b *bucket) wait(ctx context.Context, n float64) error {
	now := time.Now()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	b.tokens -= min(n, b.burst)
	if b.tokens >= 0 {
		return nil
	}
	t := time.NewTimer(time.Duration(-b.tokens / b.rate * float64(time.Second)))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// clientIP is the caller's address, from the proxy's header when one is
// trusted, else the socket's peer; site is its IPv6 /48, empty for IPv4.
func (s *Server) clientIP(r *http.Request) (ip, site string) {
	if h := s.cfg.ClientIPHeader; h != "" {
		if ip := net.ParseIP(r.Header.Get(h)); ip != nil {
			return subscriber(ip), siteOf(ip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr, ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return subscriber(ip), siteOf(ip)
	}
	return host, ""
}

// subscriber is an IPv6 address's /64: one subscriber holds the whole prefix.
func subscriber(ip net.IP) string {
	if ip.To4() != nil {
		return ip.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// siteOf is an IPv6 address's /48, the most one allocation usually spans.
func siteOf(ip net.IP) string {
	if ip.To4() != nil {
		return ""
	}
	return ip.Mask(net.CIDRMask(48, 128)).String() + "/48"
}

// admitIP reserves a socket for ip and its site, or reports that either
// already holds its share.
func (s *Server) admitIP(ip, site string, now time.Time) bool {
	s.ipMu.Lock()
	defer s.ipMu.Unlock()
	if now.Sub(s.attemptWindow) >= time.Minute {
		s.attempts = map[string]int{}
		s.attemptWindow = now
	}
	// A full table skips the budget: refusing new addresses would lock everyone out.
	if _, held := s.attempts[ip]; held || len(s.attempts) < maxAttemptIPs {
		if s.attempts[ip] >= connectionAttemptsPerMinute {
			return false
		}
		s.attempts[ip]++
	}
	if s.ips[ip] >= s.connsPerIP || site != "" && s.ips[site] >= sitesPerIP*s.connsPerIP {
		return false
	}
	s.ips[ip]++
	if site != "" {
		s.ips[site]++
	}
	return true
}

func (s *Server) releaseIP(ip, site string) {
	s.ipMu.Lock()
	defer s.ipMu.Unlock()
	for _, key := range []string{ip, site} {
		if key == "" {
			continue
		}
		if s.ips[key]--; s.ips[key] <= 0 {
			delete(s.ips, key)
		}
	}
}
