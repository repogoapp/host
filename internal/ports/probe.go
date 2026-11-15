package ports

import (
	"context"
	"crypto/tls"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// A listener shows only once it answers a web request: the Browser tab can
// open nothing else, and tooling that listens without serving pages (adb, an
// emulator, a database) drops out without a list of names to keep current.
const (
	// probeTimeout bounds one check. Loopback servers answer in milliseconds;
	// what runs this long is a socket that accepted and never replied.
	probeTimeout = 700 * time.Millisecond
	// probeRetry is how long a failed listener waits before it is checked
	// again: a dev server still compiling fails its first check.
	probeRetry = 5 * time.Second
	// probeLimit caps checks in flight at once.
	probeLimit = 32
)

// probeKey is one listener: a process can close a port and open it again, so
// the answer belongs to the pair, not the process.
type probeKey struct{ pid, port int }

type probeState struct {
	web     bool
	checked time.Time
	done    chan struct{} // open while a check runs
}

// prober remembers which listeners answered. A pass holds for the listener's
// life; a failure is checked again after probeRetry, in the background.
type prober struct {
	check func(ctx context.Context, port int) bool
	slots chan struct{}

	mu     sync.Mutex
	states map[probeKey]*probeState
}

var webProbes = newProber(answersHTTP)

func newProber(check func(context.Context, int) bool) *prober {
	return &prober{check: check, slots: make(chan struct{}, probeLimit), states: map[probeKey]*probeState{}}
}

// web reports which of listeners serve pages. It forgets listeners that are
// gone, waits for the first check of new ones, and only starts rechecks of
// failed ones, so a scan waits at most one probeTimeout, and only the first
// time a listener is seen.
func (p *prober) web(ctx context.Context, listeners []listener) map[probeKey]bool {
	now := time.Now()
	live := make(map[probeKey]bool, len(listeners))
	var waits []chan struct{}

	p.mu.Lock()
	for _, l := range listeners {
		key := probeKey{l.pid, l.port}
		live[key] = true
		state := p.states[key]
		switch {
		case state == nil:
			state = &probeState{}
			p.states[key] = state
			waits = append(waits, p.start(key, state))
		case state.done != nil:
			if state.checked.IsZero() {
				waits = append(waits, state.done) // first check already running
			}
		case !state.web && now.Sub(state.checked) >= probeRetry:
			p.start(key, state)
		}
	}
	for key, state := range p.states {
		if !live[key] && state.done == nil {
			delete(p.states, key)
		}
	}
	p.mu.Unlock()

	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[probeKey]bool, len(listeners))
	for key := range live {
		if state := p.states[key]; state != nil && state.web {
			out[key] = true
		}
	}
	return out
}

// start runs one check of key; p.mu is held.
func (p *prober) start(key probeKey, state *probeState) chan struct{} {
	done := make(chan struct{})
	state.done = done
	go func() {
		p.slots <- struct{}{}
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		web := p.check(ctx, key.port)
		cancel()
		<-p.slots

		p.mu.Lock()
		state.web, state.checked, state.done = web, time.Now(), nil
		p.mu.Unlock()
		close(done)
	}()
	return done
}

// probeClient asks once and reads nothing: no proxy from the environment, no
// redirects, no kept connections, and any certificate, since a dev server's
// is usually self-signed and only the reply's shape matters here.
var probeClient = &http.Client{
	Transport: &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// answersHTTP sends HEAD / over plain HTTP and HTTPS at once, on 127.0.0.1 as
// the browser's forwarder dials, and passes on any reply Go parses as HTTP,
// whatever its status: a 404, a login wall or a 500 is still a web server.
func answersHTTP(ctx context.Context, port int) bool {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	answers := make(chan bool, 2)
	for _, scheme := range []string{"http", "https"} {
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, scheme+"://127.0.0.1:"+strconv.Itoa(port)+"/", nil)
			if err != nil {
				answers <- false
				return
			}
			resp, err := probeClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
			answers <- err == nil
		}()
	}
	for range 2 {
		if <-answers {
			return true
		}
	}
	return false
}
