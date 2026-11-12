package host_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/handshake"
	"github.com/repogoapp/host/internal/host"
	"github.com/repogoapp/host/internal/hostclient"
	"github.com/repogoapp/host/internal/hostinfo"
	"github.com/repogoapp/host/internal/testhost"
	"github.com/repogoapp/host/internal/testwait"
)

// A host with a relay also serves its paired phones straight over the
// tailnet, with the same pinned TLS, and says so in its status. Runs only on a
// machine Tailscale is up on.
func TestServesPhonesOverTheTailnet(t *testing.T) {
	ip := hostinfo.TailnetIP()
	if ip == "" {
		t.Skip("Tailscale isn't up on this machine")
	}
	testServesDirect(t, ip, func(c *host.Config) { c.Tailnet = true }, func(n hostinfo.Network) int {
		if n.Tailscale == nil || n.Tailscale.IP != ip {
			return 0
		}
		return n.Tailscale.Port
	})
}

// With LAN on, the host serves the local network address the same way.
func TestServesPhonesOnTheLocalNetworkWhenAsked(t *testing.T) {
	ip := hostinfo.LANIP()
	if ip == "" {
		t.Skip("no local network address on this machine")
	}
	testServesDirect(t, ip, func(c *host.Config) { c.LAN = true }, func(n hostinfo.Network) int {
		if n.LANIP != ip {
			return 0
		}
		return n.LANPort
	})
}

// Without --lan the local network address is never served or reported.
func TestKeepsTheLocalNetworkClosedByDefault(t *testing.T) {
	if hostinfo.LANIP() == "" {
		t.Skip("no local network address on this machine")
	}
	relay := testhost.StartRelay(t)
	th := testhost.New(t, relay.On, func(c *host.Config) { c.DirectPort, c.Tailnet = freePort(t, "127.0.0.1"), true })
	if _, err := th.Listen(); err != nil {
		t.Fatal(err)
	}
	th.Start()
	// Start spawns every listener it will, so once the relay links, LAN would be up.
	testwait.For(t, "the host to link to the relay", func() bool { return th.Config.Host.Status().RelayConnected })
	if port := th.Config.Host.Status().Network.LANPort; port != 0 {
		t.Fatalf("lan_port %d without --lan", port)
	}
}

func testServesDirect(t *testing.T, ip string, adjust func(*host.Config), served func(hostinfo.Network) int) {
	t.Helper()
	port := freePort(t, ip)
	relay := testhost.StartRelay(t)
	th := testhost.New(t, relay.On, func(c *host.Config) { c.DirectPort = port; adjust(c) })
	if _, err := th.Listen(); err != nil {
		t.Fatal(err)
	}
	th.Start()
	testwait.For(t, "the host to serve "+ip, func() bool { return served(th.Config.Host.Status().Network) == port })

	phone, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := th.Devices.Add(device.Peer{ID: phone.ID, Public: phone.Public, Label: "Phone", Client: device.Client{Platform: "ios"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := hostclient.Dial(ctx, hostclient.Config{
		URL: "wss://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/ws", TLS: testhost.PinnedTLS(th.Pin(t)),
		Identity: phone, Role: handshake.RoleClient, Platform: "ios",
	})
	if err != nil {
		t.Fatalf("dial %s: %v", ip, err)
	}
	defer c.Close()
	var status hostinfo.Status
	if err := c.Call(ctx, "host.status", nil, &status); err != nil {
		t.Fatalf("host.status over %s: %v", ip, err)
	}
}

// freePort is a port nothing listens on at ip just now.
func freePort(t *testing.T, ip string) int {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
