package ports

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, port, _ := net.SplitHostPort(addr)
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAnswersHTTPPassesWebServersOfAnyStatus(t *testing.T) {
	notFound := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	plain := httptest.NewServer(notFound)
	defer plain.Close()
	secure := httptest.NewTLSServer(notFound) // self-signed
	defer secure.Close()

	for name, addr := range map[string]string{"http": plain.Listener.Addr().String(), "https": secure.Listener.Addr().String()} {
		if !answersHTTP(context.Background(), portOf(t, addr)) {
			t.Errorf("%s server not recognised", name)
		}
	}
}

func TestAnswersHTTPFailsASilentSocketWithinTheTimeout(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // accepts and never replies, as adb does to HTTP
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	start := time.Now()
	if answersHTTP(ctx, portOf(t, ln.Addr().String())) {
		t.Fatal("silent socket passed as a web server")
	}
	if waited := time.Since(start); waited > probeTimeout+300*time.Millisecond {
		t.Fatalf("waited %v, want about %v", waited, probeTimeout)
	}
}

func TestProberWaitsForNewListenersAndRemembersThem(t *testing.T) {
	var checks atomic.Int32
	p := newProber(func(_ context.Context, port int) bool {
		checks.Add(1)
		return port == 3000
	})
	listeners := []listener{{port: 3000, pid: 1}, {port: 5037, pid: 2}}

	web := p.web(context.Background(), listeners)
	if !web[probeKey{1, 3000}] || web[probeKey{2, 5037}] {
		t.Fatalf("web = %v, want only 3000", web)
	}
	p.web(context.Background(), listeners)
	if n := checks.Load(); n != 2 {
		t.Fatalf("checks = %d, want 2: a second scan inside the retry window checks nothing", n)
	}

	p.web(context.Background(), listeners[:1]) // 5037 closed
	p.mu.Lock()
	_, kept := p.states[probeKey{2, 5037}]
	p.mu.Unlock()
	if kept {
		t.Fatal("closed listener still remembered")
	}
}

func TestProberRechecksAFailureInTheBackground(t *testing.T) {
	var ready atomic.Bool
	release := make(chan struct{})
	p := newProber(func(context.Context, int) bool {
		if ready.Load() {
			<-release // the recheck is still running while the scan returns
		}
		return ready.Load()
	})
	listeners := []listener{{port: 3000, pid: 1}}
	key := probeKey{1, 3000}

	if p.web(context.Background(), listeners)[key] {
		t.Fatal("passed before the server was ready")
	}
	ready.Store(true) // the dev server finished compiling
	p.mu.Lock()
	p.states[key].checked = time.Now().Add(-probeRetry)
	p.mu.Unlock()

	if p.web(context.Background(), listeners)[key] {
		t.Fatal("a recheck held up the scan instead of running in the background")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for !p.web(context.Background(), listeners)[key] {
		if time.Now().After(deadline) {
			t.Fatal("recheck never passed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
