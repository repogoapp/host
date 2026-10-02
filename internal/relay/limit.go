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
	// included, so one machine cannot fill the relay. Generous because carrier
	// NAT puts many phones behind one address.
	defaultConnsPerIP = 64
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

// clientIP is the caller's address: the proxy's header when one is trusted,
// else the socket's peer.
func (s *Server) clientIP(r *http.Request) string {
	if h := s.cfg.ClientIPHeader; h != "" {
		if ip := r.Header.Get(h); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// admitIP reserves a socket for ip, or reports that it already holds its share.
func (s *Server) admitIP(ip string) bool {
	s.ipMu.Lock()
	defer s.ipMu.Unlock()
	if s.ips[ip] >= s.connsPerIP {
		return false
	}
	s.ips[ip]++
	return true
}

func (s *Server) releaseIP(ip string) {
	s.ipMu.Lock()
	defer s.ipMu.Unlock()
	if s.ips[ip]--; s.ips[ip] <= 0 {
		delete(s.ips, ip)
	}
}
