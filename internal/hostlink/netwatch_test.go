package hostlink

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func ipNets(ips ...string) []net.Addr {
	var list []net.Addr
	for _, ip := range ips {
		list = append(list, &net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(24, 32)})
	}
	return list
}

// Switching from home Wi-Fi to a hotspot takes the socket's address away;
// the watch sees it on the next check and gives up on the socket.
func TestWatchAddressFiresWhenTheAddressLeaves(t *testing.T) {
	var current atomic.Value
	current.Store(ipNets("127.0.0.1", "192.168.1.102"))
	addrs := func() ([]net.Addr, error) { return current.Load().([]net.Addr), nil }

	ticks := make(chan time.Time)
	gone := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchAddress(t.Context(), net.ParseIP("192.168.1.102"), ticks, addrs, func() { close(gone) })
	}()

	// Unbuffered: the second send only lands once the first check finished.
	ticks <- time.Now()
	ticks <- time.Now()
	select {
	case <-gone:
		t.Fatal("fired while the address was still on the machine")
	default:
	}

	// The second check may already see the new list, so tick until it fires.
	current.Store(ipNets("127.0.0.1", "172.20.10.4"))
	for {
		select {
		case ticks <- time.Now():
		case <-gone:
			<-done
			return
		}
	}
}

func TestWatchAddressStopsWithTheSession(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchAddress(ctx, net.ParseIP("10.0.0.2"), make(chan time.Time), nil, func() {
			t.Error("fired after the session ended")
		})
	}()
	cancel()
	<-done
}

func TestDialerRemembersTheLocalAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	d := newDialer()
	resp, err := d.client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if ip := d.localIP(); !ip.IsLoopback() {
		t.Fatalf("local ip = %v, want loopback", ip)
	}
}
