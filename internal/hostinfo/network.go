package hostinfo

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// publicIPTTL bounds how stale a status's public address may be before it asks for a new one.
const publicIPTTL = 5 * time.Minute

var errUnexpectedIP = errors.New("public IP lookup: unexpected answer")

// Network is where this machine sits: its addresses and the port the host serves on.
type Network struct {
	// LANIP is the address of the interface the default route leaves by.
	LANIP    string `json:"lan_ip,omitempty"`
	PublicIP string `json:"public_ip,omitempty"`
	// LocalHostname is the Mac's Bonjour name, "Jerricks-MacBook-Pro.local"; absent off macOS.
	LocalHostname string `json:"local_hostname,omitempty"`
	Port          int    `json:"port"`
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
	return Network{LANIP: lanIP(), LocalHostname: localHostname()}
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
	return n
}

// lanIP dials nothing: connecting a UDP socket only picks the route and its source address.
func lanIP() string {
	conn, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
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
