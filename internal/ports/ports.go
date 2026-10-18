package ports

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/files"
)

// Port is one TCP socket listening on this machine, tagged with its owning
// process and, when portless is serving it, its `.localhost` name.
type Port struct {
	Port     int    `json:"port"`
	Command  string `json:"command"`
	Cwd      string `json:"cwd"`
	Portless string `json:"portless"`
}

// listener is one socket as the platform reports it; the pid stays here.
type listener struct {
	port    int
	pid     int
	command string
	cwd     string
}

// ListPorts enumerates listening sockets (see ports_*.go), filtered to servers
// started inside dir when given. Failure yields an empty list.
func ListPorts(ctx context.Context, dir string) []Port {
	return ports(listPlatformPorts(ctx), os.Getpid(), portlessRoutes(), dir)
}

// ports leaves out self, the host's own listener, which isn't a dev server.
func ports(listeners []listener, self int, routes map[int]string, dir string) []Port {
	kept := make([]Port, 0, len(listeners))
	for _, l := range listeners {
		if l.pid == self || (dir != "" && !files.Within(dir, l.cwd)) {
			continue
		}
		kept = append(kept, Port{Port: l.port, Command: l.command, Cwd: l.cwd, Portless: routes[l.port]})
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Port < kept[j].Port })
	return kept
}

// ErrSelf refuses the host's own listener: a phone clearing a stuck dev
// server must not be able to take its own connection down.
var ErrSelf = errkind.New(errkind.Invalid, "ports: that port is the host's own")

// KillPort signals every listener on port, resolved now because a caller's scan
// may be stale, and counts the signals the OS accepted: none when nothing was
// listening or every listener refused. IPv4 and IPv6 rows are separate listeners.
func KillPort(ctx context.Context, port int, force bool) (int, error) {
	if port <= 0 {
		return 0, nil
	}
	killed := 0
	for _, l := range listPlatformPorts(ctx) {
		if l.port != port || l.pid <= 0 {
			continue
		}
		if l.pid == os.Getpid() {
			return 0, ErrSelf
		}
		if signalProcess(l.pid, force) == nil {
			killed++
		}
	}
	return killed, nil
}

// signalProcess asks a pid to stop, through os.Process rather than
// syscall.Kill, which Windows lacks. Windows has no graceful terminate for an
// unrelated process, so SIGTERM fails there and the fallback terminates.
func signalProcess(pid int, force bool) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if force {
		return process.Kill()
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return process.Kill()
	}
	return nil
}

// portlessRoutes reads portless's live `{hostname, port}` table from
// `$PORTLESS_STATE_DIR/routes.json` (default `~/.portless`). A name holds only
// while portless serves the port; a missing file yields an empty map.
func portlessRoutes() map[int]string {
	dir := os.Getenv("PORTLESS_STATE_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		dir = filepath.Join(home, ".portless")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "routes.json"))
	if err != nil {
		return nil
	}
	var entries []struct {
		Hostname string `json:"hostname"`
		Port     int    `json:"port"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	out := make(map[int]string, len(entries))
	for _, entry := range entries {
		if entry.Hostname != "" && entry.Port > 0 {
			out[entry.Port] = entry.Hostname
		}
	}
	return out
}
