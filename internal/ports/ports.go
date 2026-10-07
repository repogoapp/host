package ports

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/files"
)

// Port is one TCP socket listening on this machine, tagged with its owning
// process and, when portless is serving it, its `.localhost` name.
type Port struct {
	Port     int    `json:"port"`
	PID      int    `json:"pid"`
	Command  string `json:"command"`
	Cwd      string `json:"cwd"`
	Portless string `json:"portless"`
}

// ListPorts enumerates listening sockets (see ports_*.go), filtered to servers
// started inside dir when given. Failure yields an empty list: a machine
// without lsof still forwards a typed port.
func ListPorts(ctx context.Context, dir string) []Port {
	ports := listPlatformPorts(ctx)
	routes := portlessRoutes()
	kept := make([]Port, 0, len(ports))
	for _, p := range ports {
		p.Portless = routes[p.Port]
		if dir == "" || files.Within(dir, p.Cwd) {
			kept = append(kept, p)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Port < kept[j].Port })
	return kept
}

// Killed is what KillPort stopped.
type Killed struct {
	// Empty when nothing was listening, or every listener refused the signal.
	PIDs []int `json:"killed_pids" wire:"array"`
	// Owning process name, for the client's confirmation copy.
	Command string `json:"command,omitempty"`
}

// ErrSelf refuses the host's own listener: a phone clearing a stuck dev
// server must not be able to take its own connection down.
var ErrSelf = errkind.New(errkind.Invalid, "ports: that port is the host's own")

// KillPort signals every listener on port, resolved now because a caller's scan
// may be stale. IPv4 and IPv6 rows of one server are separate listeners; PIDs
// reports only the signals the OS accepted.
func KillPort(ctx context.Context, port int, force bool) (Killed, error) {
	out := Killed{PIDs: []int{}}
	if port <= 0 {
		return out, nil
	}
	for _, p := range listPlatformPorts(ctx) {
		if p.Port != port || p.PID <= 0 {
			continue
		}
		if p.PID == os.Getpid() {
			return Killed{PIDs: []int{}}, ErrSelf
		}
		if out.Command == "" {
			out.Command = p.Command
		}
		if signalProcess(p.PID, force) == nil {
			out.PIDs = append(out.PIDs, p.PID)
		}
	}
	return out, nil
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
