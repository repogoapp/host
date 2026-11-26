package tunnelgateway

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// The gateway is one small machine, so what any visitor or host can make it
// hold is capped: these, the response cache and the hub's per-host slots.
const (
	// proofReadLimit caps a socket's messages until its key is proven; a
	// Hello and a Proof are well under 1 KB.
	proofReadLimit = 64 << 10
	// socketsPerIP caps host sockets from one address, room for a household
	// of computers and containers.
	socketsPerIP = 32
	// bodyBudgetBytes caps the request bodies held at once, across visitors.
	bodyBudgetBytes = 128 << 20
	// bodyReadTimeout bounds how long a visitor may take to send a body.
	bodyReadTimeout = 60 * time.Second
	// maxChunk caps one streamed body chunk from a host, which sends 512 KB.
	maxChunk = 1 << 20
	// missFor is how long a name with no tunnel is answered without a lookup;
	// a tunnel is opened before its link is shared, so it is never one.
	missFor = 5 * time.Second
	// maxMisses caps the names remembered, so a flood of them stays small.
	maxMisses = 10_000
)

var errBodyBudget = errors.New("the gateway is holding as many request bodies as it can")

// bodyBudget counts the request body bytes the gateway holds. Its zero value
// holds nothing yet.
type bodyBudget struct {
	mu   sync.Mutex
	used int64
}

func (b *bodyBudget) take(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > bodyBudgetBytes {
		return false
	}
	b.used += n
	return true
}

func (b *bodyBudget) give(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

// readBody reads r's body within the budget and a deadline; done gives its
// bytes back once the body has been sent on.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) (body []byte, done func(), err error) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()

	var held int64
	done = func() { s.bodies.give(held) }
	buf := make([]byte, 32<<10)
	for {
		n, rerr := r.Body.Read(buf)
		if n > 0 {
			if !s.bodies.take(int64(n)) {
				done()
				return nil, func() {}, errBodyBudget
			}
			held += int64(n)
			body = append(body, buf[:n]...)
		}
		if rerr == io.EOF {
			return body, done, nil
		}
		if rerr != nil {
			done()
			return nil, func() {}, rerr
		}
	}
}

// socketCounts counts host sockets per address. Its zero value is empty.
type socketCounts struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *socketCounts) take(ip string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	if c.counts[ip] >= socketsPerIP {
		return false
	}
	c.counts[ip]++
	return true
}

func (c *socketCounts) give(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[ip]--; c.counts[ip] <= 0 {
		delete(c.counts, ip)
	}
}

// missCache remembers names with no tunnel for missFor. Its zero value is empty.
type missCache struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (c *missCache) recent(name string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Before(c.until[name])
}

func (c *missCache) add(name string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.until) >= maxMisses {
		for n, t := range c.until {
			if !now.Before(t) {
				delete(c.until, n)
			}
		}
	}
	if len(c.until) >= maxMisses || c.until == nil {
		c.until = map[string]time.Time{}
	}
	c.until[name] = now.Add(missFor)
}
