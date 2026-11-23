// Package remotehost finds the other RepoGo hosts this user can reach over
// SSH: the hosts in ~/.ssh/config and the machines on their tailnet, each
// checked with `repogo status --json` on the other end. SSH is the only
// credential; nothing here listens or announces.
package remotehost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/hostinfo"
	"github.com/repogoapp/host/internal/syscmd"
)

// Host is one machine discovery found, usable or not.
type Host struct {
	// ID is what SSH dials and what agents name it by: the ~/.ssh/config
	// alias, else the tailnet name's first label.
	ID string `json:"id"`
	// HostID, Name and Version are what `repogo status` answered; empty
	// unless State is Ready.
	HostID  string `json:"host_id,omitempty"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	State   State  `json:"state"`
	// Reason says, for a host that isn't Ready, what the user can do.
	Reason string `json:"reason,omitempty"`
}

// State is whether a host can run RepoGo's tools from here, and if not, why.
type State string

const (
	Ready        State = "ready"
	Offline      State = "offline"
	NoKey        State = "no_key"
	NotInstalled State = "not_installed"
	NotRunning   State = "not_running"
	TooOld       State = "too_old"
)

// statusCommand runs on the other host. A non-interactive SSH session's PATH
// lacks ~/.repogo/bin, so the path is spelled out.
const statusCommand = "~/.repogo/bin/repogo status --json"

// checkTimeout bounds one host's SSH call, login included.
const checkTimeout = 5 * time.Second

// Deps is how discovery reads this machine and reaches others; System is the real one.
type Deps struct {
	// Self is this host's ID, left out of the answer when a name reaches it.
	Self string
	// SSHConfig is ~/.ssh/config's contents; empty when there is none.
	SSHConfig func() []byte
	// Resolve is the address SSH dials for an alias (`ssh -G`'s hostname).
	Resolve func(ctx context.Context, alias string) string
	// Tailnet is `tailscale status --json`; empty when Tailscale isn't up.
	Tailnet func(ctx context.Context) []byte
	// Run runs command on target over SSH, never prompting: its output and
	// exit code, -1 when it didn't finish.
	Run func(ctx context.Context, target, command string) (stdout, stderr []byte, code int)
}

// candidate is one name to check.
type candidate struct {
	id, target string
	// offline is a machine Tailscale reports offline: not worth an SSH call.
	offline bool
}

// Discover checks every candidate at once, so it takes as long as the
// slowest; a ~/.ssh/config host comes before a tailnet peer that is the same
// machine.
func Discover(ctx context.Context, d Deps) []Host {
	cands := candidates(ctx, d)
	found := make([]Host, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			found[i] = check(ctx, d, c)
		}()
	}
	wg.Wait()
	return merge(found, d.Self)
}

// candidates is the ~/.ssh/config aliases, then the tailnet peers none of
// them already dials. A config alias whose address is an offline peer is
// offline too.
func candidates(ctx context.Context, d Deps) []candidate {
	peers := parseTailnet(d.Tailnet(ctx))
	taken := map[string]bool{}
	var out []candidate
	for _, alias := range parseSSHConfig(d.SSHConfig()) {
		addr := d.Resolve(ctx, alias)
		if codeHosts[strings.ToLower(addr)] {
			continue
		}
		c := candidate{id: alias, target: alias}
		for _, p := range peers {
			if p.has(addr) {
				c.offline = !p.Online
				taken[p.DNSName] = true
			}
		}
		out = append(out, c)
	}
	for _, p := range peers {
		if taken[p.DNSName] || p.DNSName == "" {
			continue
		}
		name := strings.TrimSuffix(p.DNSName, ".")
		id, _, _ := strings.Cut(name, ".")
		out = append(out, candidate{id: id, target: name, offline: !p.Online})
	}
	return out
}

// codeHosts are where a ~/.ssh/config alias points to push code, not to a
// computer: left out rather than listed as a host with no shell.
var codeHosts = map[string]bool{
	"github.com": true, "ssh.github.com": true, "gitlab.com": true,
	"altssh.gitlab.com": true, "bitbucket.org": true, "altssh.bitbucket.org": true,
}

// check asks one host for its status; how the call fails says what's missing.
func check(ctx context.Context, d Deps, c candidate) Host {
	h := Host{ID: c.id}
	if c.offline {
		h.State, h.Reason = Offline, "offline in Tailscale"
		return h
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	stdout, stderr, code := d.Run(ctx, c.target, statusCommand)
	if h.State, h.Reason = classify(code, string(stderr)); h.State != Ready {
		return h
	}
	var status hostinfo.Status
	if json.Unmarshal(stdout, &status) != nil || status.HostID == "" {
		h.State, h.Reason = TooOld, "update RepoGo there"
		return h
	}
	h.HostID, h.Name, h.Version = status.HostID, status.Name, status.Version
	return h
}

// classify reads an SSH call's exit code and stderr: 255 is SSH's own
// failure, 127 the remote shell's command not found, 1 the repogo CLI's error.
func classify(code int, stderr string) (State, string) {
	switch {
	case code == 0:
		return Ready, ""
	case code == 255 && strings.Contains(stderr, "Permission denied"):
		return NoKey, "no SSH key that works there"
	case code == 255 && strings.Contains(stderr, "Host key verification failed"):
		return NoKey, "run ssh there once to trust its host key"
	case code == 127:
		return NotInstalled, "RepoGo isn't installed"
	case strings.Contains(stderr, "not running"):
		return NotRunning, "run repogo start there"
	case strings.Contains(stderr, "unknown flag"):
		return TooOld, "update RepoGo there"
	}
	return Offline, firstLine(stderr)
}

// merge keeps one entry per host ID, the first (a ~/.ssh/config alias before
// a tailnet name), and drops this host. Hosts that didn't answer stay, so the
// list can say why.
func merge(found []Host, self string) []Host {
	seen := map[string]bool{}
	out := []Host{}
	for _, h := range found {
		if h.HostID != "" {
			if h.HostID == self || seen[h.HostID] {
				continue
			}
			seen[h.HostID] = true
		}
		out = append(out, h)
	}
	return out
}

// parseSSHConfig is the first name of each Host line that isn't a pattern.
// Include and Match are not followed.
func parseSSHConfig(data []byte) []string {
	var out []string
	lines := bufio.NewScanner(bytes.NewReader(data))
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		key, rest, ok := strings.Cut(strings.Replace(line, "=", " ", 1), " ")
		if !ok || !strings.EqualFold(key, "Host") {
			continue
		}
		for _, name := range strings.Fields(rest) {
			if !strings.ContainsAny(name, "*?!") {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// peer is one machine in `tailscale status --json`.
type peer struct {
	DNSName      string
	OS           string
	Online       bool
	TailscaleIPs []string
}

// has reports whether addr is this peer's name or one of its addresses.
func (p peer) has(addr string) bool {
	if addr == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSuffix(p.DNSName, "."), strings.TrimSuffix(addr, ".")) {
		return true
	}
	for _, ip := range p.TailscaleIPs {
		if ip == addr {
			return true
		}
	}
	return false
}

// computerOS is the tailnet OSes a host runs on; phones are left out.
var computerOS = map[string]bool{"macOS": true, "linux": true, "windows": true}

// parseTailnet is the computers among the tailnet's peers; none for output it can't read.
func parseTailnet(data []byte) []peer {
	var status struct {
		Peer map[string]peer
	}
	if len(data) == 0 || json.Unmarshal(data, &status) != nil {
		return nil
	}
	var out []peer
	for _, p := range status.Peer {
		if computerOS[p.OS] {
			out = append(out, p)
		}
	}
	// The map's order is random; a stable list keeps agents' IDs in place.
	slices.SortFunc(out, func(a, b peer) int { return strings.Compare(a.DNSName, b.DNSName) })
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// System is discovery on this machine: home's ~/.ssh/config, the tailscale
// CLI, and ssh in batch mode.
func System(self, home string) Deps {
	return Deps{
		Self: self,
		SSHConfig: func() []byte {
			data, _ := os.ReadFile(filepath.Join(home, ".ssh", "config"))
			return data
		},
		Resolve: func(ctx context.Context, alias string) string {
			out, err := syscmd.Output(ctx, 2*time.Second, "ssh", "-G", alias)
			if err != nil {
				return ""
			}
			for _, line := range strings.Split(out, "\n") {
				if host, ok := strings.CutPrefix(line, "hostname "); ok {
					return strings.TrimSpace(host)
				}
			}
			return ""
		},
		Tailnet: func(ctx context.Context) []byte {
			for _, cli := range hostinfo.TailscaleCLIs {
				path, err := exec.LookPath(cli)
				if err != nil {
					continue
				}
				// Stdout alone: the CLI warns on stderr when it and the daemon differ.
				ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				out, err := exec.CommandContext(ctx, path, "status", "--json").Output()
				cancel()
				if err == nil {
					return out
				}
			}
			return nil
		},
		Run: func(ctx context.Context, target, command string) ([]byte, []byte, int) {
			var stdout, stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", target, command)
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			var exit *exec.ExitError
			switch {
			case err == nil:
				return stdout.Bytes(), stderr.Bytes(), 0
			case errors.As(err, &exit) && ctx.Err() == nil:
				return stdout.Bytes(), stderr.Bytes(), exit.ExitCode()
			}
			return stdout.Bytes(), stderr.Bytes(), -1
		},
	}
}
