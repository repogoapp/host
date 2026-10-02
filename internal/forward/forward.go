// Package forward carries a client to dev servers on this host's loopback over
// the connection the host already holds. Pipes relay raw bytes, so pages,
// upgrades and streams all ride them; Fetch is one buffered request and reply.
package forward

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/repogo/host/internal/errkind"
)

var ErrPort = errkind.New(errkind.Invalid, "forward: port must be between 1 and 65535")

// Loopback is a client that can only reach this machine's 127.0.0.1: the port
// is caller-supplied, and a client that would dial anywhere turns a proxy
// into an open one running as the user. timeout 0 is none, for streams.
func Loopback(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		// Redirects are the caller's to follow: it owns the address bar.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dialLoopback(ctx, addr)
			},
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}

// HopByHop headers describe one connection, not the message. Transfer-Encoding
// especially: a proxied body is already decoded.
func HopByHop(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length":
		return true
	}
	return false
}

// dialLoopback rewrites any destination to 127.0.0.1, keeping only the port.
// Guarded here because this is the only place a redirect or header could
// produce a socket.
func dialLoopback(ctx context.Context, addr string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("forward: bad address %q: %w", addr, err)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp4", "127.0.0.1:"+port)
}
