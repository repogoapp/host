package hostinfo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// publicIPTTL bounds how stale a status's public address may be before it asks for a new one.
const publicIPTTL = 5 * time.Minute

var errUnexpectedIP = errors.New("public IP lookup: unexpected answer")

// Network is where this machine sits: its addresses and the port the host serves on.
type Network struct {
	// LANIP is the address of the interface the default route leaves by.
	LANIP string `json:"lan_ip,omitempty"`
	// LANPort is where the host serves phones on LANIP; absent unless started with --lan.
	LANPort  int    `json:"lan_port,omitempty"`
	PublicIP string `json:"public_ip,omitempty"`
	// LocalHostname is the Mac's Bonjour name, "Jerricks-MacBook-Pro.local"; absent off macOS.
	LocalHostname string `json:"local_hostname,omitempty"`
	Port          int    `json:"port"`
	// Tailscale is this machine on its tailnet; absent when Tailscale isn't up.
	Tailscale *Tailscale `json:"tailscale,omitempty"`
}

// Tailscale is the machine's tailnet address and, with MagicDNS on, its name,
// "jerricks-mac.tail1234.ts.net".
type Tailscale struct {
	IP   string `json:"ip"`
	Name string `json:"name,omitempty"`
	// Port is where the host serves phones on IP; absent until it listens there.
	Port int `json:"port,omitempty"`
}

// SetLAN records the local network address the host listens on; zero port when none.
func (s *Service) SetLAN(ip string, port int) {
	s.mu.Lock()
	s.lanIP, s.lanPort = ip, port
	s.mu.Unlock()
}

// SetTailnet records the tailnet address the host listens on; zero port when none.
func (s *Service) SetTailnet(ip string, port int) {
	s.mu.Lock()
	s.tailnetIP, s.tailnetPort = ip, port
	s.mu.Unlock()
}

// SetPort records the port Listen bound, which differs from the configured one when that was 0.
func (s *Service) SetPort(port int) {
	s.mu.Lock()
	s.port = port
	s.mu.Unlock()
}

// Port is what SetPort recorded, or zero before Listen.
func (s *Service) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// WatchPublicIP looks the public address up at start and again whenever a
// status finds it stale, so a status never waits on the network.
func (s *Service) WatchPublicIP(ctx context.Context) {
	for {
		lookup, cancel := context.WithTimeout(ctx, 3*time.Second)
		ip, err := s.d.PublicIP(lookup)
		cancel()
		if err != nil {
			ip = ""
		}
		s.mu.Lock()
		s.publicIP, s.publicAt = ip, time.Now()
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-s.publicStale:
		}
	}
}

// localNetwork is the part read from this machine; Status reads it before taking mu.
func localNetwork() Network {
	return Network{LANIP: LANIP(), LocalHostname: localHostname(), Tailscale: tailscale()}
}

// tailnetRange is the CGNAT block Tailscale gives every device an address in.
var tailnetRange = netip.MustParsePrefix("100.64.0.0/10")

func tailscale() *Tailscale {
	ip, name := Tailnet()
	if ip == "" {
		return nil
	}
	return &Tailscale{IP: ip, Name: name}
}

// TailnetIP is this machine's Tailscale address; empty when Tailscale isn't up.
func TailnetIP() string {
	ip, _ := Tailnet()
	return ip
}

// tailnetRecheck is how long Tailnet trusts its last answer for one address.
const tailnetRecheck = time.Minute

// tailnet is Tailnet's last answer, so a status or the 15 s check doesn't
// ask Tailscale again each time.
var tailnet struct {
	sync.Mutex
	candidate, name string
	confirmed       bool
	at              time.Time
}

// Tailnet is this machine's Tailscale address and name, vouched for by
// Tailscale's resolver or CLI: carriers and other VPNs use 100.64/10 too, and
// serving their address would open the port to strangers. Empty otherwise.
func Tailnet() (ip, name string) {
	candidate := tailnetCandidate()
	if candidate == "" {
		return "", ""
	}
	tailnet.Lock()
	defer tailnet.Unlock()
	if candidate != tailnet.candidate || time.Since(tailnet.at) > tailnetRecheck {
		name := magicDNSName(candidate)
		tailnet.candidate, tailnet.name, tailnet.at = candidate, name, time.Now()
		tailnet.confirmed = name != "" || cliTailnetIP() == candidate
	}
	if !tailnet.confirmed {
		return "", ""
	}
	return candidate, tailnet.name
}

// tailnetCandidate is the first interface address in Tailscale's block.
func tailnetCandidate() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		prefix, err := netip.ParsePrefix(a.String())
		if err == nil && tailnetRange.Contains(prefix.Addr()) {
			return prefix.Addr().String()
		}
	}
	return ""
}

// tailscaleCLIs is where the tailscale command may be: a service's PATH can
// lack Homebrew's, and the Mac app carries its own.
var tailscaleCLIs = []string{
	"tailscale", "/opt/homebrew/bin/tailscale", "/usr/local/bin/tailscale",
	"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
}

// cliTailnetIP is the IPv4 address the tailscale CLI reports; empty without one.
func cliTailnetIP() string {
	for _, cli := range tailscaleCLIs {
		path, err := exec.LookPath(cli)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		out, err := exec.CommandContext(ctx, path, "ip", "-4").Output()
		cancel()
		if err == nil {
			return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		}
	}
	return ""
}

// magicDNSName is the reverse lookup of ip at Tailscale's resolver; empty
// when MagicDNS is off. A status waits at most magicDNSTimeout for it.
func magicDNSName(ip string) string {
	ctx, cancel := context.WithTimeout(context.Background(), magicDNSTimeout)
	defer cancel()
	names, err := magicDNS.LookupAddr(ctx, ip)
	if err != nil || len(names) == 0 {
		return ""
	}
	return strings.TrimSuffix(names[0], ".")
}

const magicDNSTimeout = 300 * time.Millisecond

var magicDNS = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", "100.100.100.100:53")
	},
}

// network adds the port and the last public lookup to n; the caller holds mu.
func (s *Service) network(n Network) Network {
	if !s.publicAt.IsZero() && time.Since(s.publicAt) > publicIPTTL {
		select {
		case s.publicStale <- struct{}{}:
		default:
		}
	}
	n.PublicIP, n.Port = s.publicIP, s.port
	if n.LANIP != "" && n.LANIP == s.lanIP {
		n.LANPort = s.lanPort
	}
	if n.Tailscale != nil && n.Tailscale.IP == s.tailnetIP {
		n.Tailscale.Port = s.tailnetPort
	}
	return n
}

// containerCLIs are where Apple's container CLI may be; a service's PATH may
// lack /usr/local/bin.
var containerCLIs = []string{"container", "/usr/local/bin/container"}

// ContainerNetworks are this machine's Apple container networks it is on: each
// network's gateway, which a local interface holds only while a container runs
// there, with the network's prefix length. Empty without the container CLI.
func ContainerNetworks(ctx context.Context) []netip.Prefix {
	for _, cli := range containerCLIs {
		path, err := exec.LookPath(cli)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		out, err := exec.CommandContext(ctx, path, "network", "list", "--format", "json").Output()
		cancel()
		var networks []struct {
			Status struct {
				Gateway string `json:"ipv4Gateway"`
				Subnet  string `json:"ipv4Subnet"`
			} `json:"status"`
		}
		if err != nil || json.Unmarshal(out, &networks) != nil {
			return nil
		}
		var found []netip.Prefix
		for _, n := range networks {
			ip, err := netip.ParseAddr(n.Status.Gateway)
			subnet, serr := netip.ParsePrefix(n.Status.Subnet)
			if err == nil && serr == nil && ip.IsPrivate() && subnet.Contains(ip) && interfaceOf(ip) != nil {
				found = append(found, netip.PrefixFrom(ip, subnet.Bits()))
			}
		}
		return found
	}
	return nil
}

// LANIP is the address the default route leaves by, when that is the local
// network. It dials nothing: connecting a UDP socket only picks the route and
// its source address.
func LANIP() string {
	conn, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	ip, ok := netip.AddrFromSlice(conn.LocalAddr().(*net.UDPAddr).IP.To4())
	if !ok || !isLocalNetwork(ip, interfaceOf(ip)) {
		return ""
	}
	return ip.String()
}

// isLocalNetwork is a private address on a real network interface. A public
// address would put the port on the internet, and a tunnel's (a work VPN, an
// exit node) on everyone else on that VPN.
func isLocalNetwork(ip netip.Addr, iface *net.Interface) bool {
	if !ip.IsPrivate() || tailnetRange.Contains(ip) || iface == nil || iface.Flags&net.FlagPointToPoint != 0 {
		return false
	}
	for _, tunnel := range []string{"utun", "tun", "tap", "ppp", "ipsec", "wg", "tailscale", "zt"} {
		if strings.HasPrefix(iface.Name, tunnel) {
			return false
		}
	}
	return true
}

// interfaceOf is the interface holding ip; nil when none does.
func interfaceOf(ip netip.Addr) *net.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for i := range ifaces {
		addrs, _ := ifaces[i].Addrs()
		for _, a := range addrs {
			if prefix, err := netip.ParsePrefix(a.String()); err == nil && prefix.Addr() == ip {
				return &ifaces[i]
			}
		}
	}
	return nil
}

func localHostname() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("/usr/sbin/scutil", "--get", "LocalHostName").Output()
	name := strings.TrimSpace(string(out))
	if err != nil || name == "" {
		return ""
	}
	return name + ".local"
}

var publicIPClient = &http.Client{Timeout: 3 * time.Second}

// LookupPublicIP asks a plaintext echo service which address this machine's requests leave from.
func LookupPublicIP(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return "", err
	}
	resp, err := publicIPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(strings.TrimSpace(string(body)))
	if resp.StatusCode != http.StatusOK || ip == nil {
		return "", errUnexpectedIP
	}
	return ip.String(), nil
}
