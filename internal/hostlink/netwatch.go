package hostlink

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// addressCheckEvery is how often the link checks that its socket's address
// still belongs to this machine.
const addressCheckEvery = 2 * time.Second

// dialer opens the relay socket and remembers its local address, which
// websocket.Dial does not expose.
type dialer struct {
	client *http.Client

	mu    sync.Mutex
	local net.Addr
}

func newDialer() *dialer {
	d := &dialer{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	var nd net.Dialer
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := nd.DialContext(ctx, network, addr)
		if err == nil {
			d.mu.Lock()
			d.local = conn.LocalAddr()
			d.mu.Unlock()
		}
		return conn, err
	}
	d.client = &http.Client{Transport: transport}
	return d
}

// localIP is the address of the last socket this dialer opened.
func (d *dialer) localIP() net.IP {
	d.mu.Lock()
	defer d.mu.Unlock()
	if tcp, ok := d.local.(*net.TCPAddr); ok {
		return tcp.IP
	}
	return nil
}

// watchAddress calls gone once ip leaves this machine's interfaces. Changing
// networks kills a socket without a close or reset, so without this the link
// waits out its pings, a minute or more, before redialling.
func watchAddress(ctx context.Context, ip net.IP, ticks <-chan time.Time, addrs func() ([]net.Addr, error), gone func()) {
	if ip == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		list, err := addrs()
		if err != nil {
			continue
		}
		if !hasIP(list, ip) {
			gone()
			return
		}
	}
}

func hasIP(list []net.Addr, ip net.IP) bool {
	for _, a := range list {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}
