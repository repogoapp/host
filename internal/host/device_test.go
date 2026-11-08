package host

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestDeviceListenerTakesOnlyItsNetwork(t *testing.T) {
	for network, want := range map[string]bool{"127.0.0.0/8": true, "10.0.0.0/8": false} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		only := callersFrom{l, netip.MustParsePrefix(network)}
		accepted := make(chan net.Conn, 1)
		go func() {
			if c, err := only.Accept(); err == nil {
				accepted <- c
			}
		}()
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		var got bool
		select {
		case served := <-accepted:
			got = true
			served.Close()
		case <-time.After(time.Second):
		}
		if got != want {
			t.Errorf("a loopback caller on a listener for %s: accepted %v, want %v", network, got, want)
		}
		if !want {
			// A refused caller is hung up on, not left waiting.
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := c.Read(make([]byte, 1)); err == nil {
				t.Errorf("a refused caller read data")
			}
		}
		c.Close()
		l.Close()
	}
}
