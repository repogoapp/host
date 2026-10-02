//go:build !linux && !windows

package ports

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/repogo/host/internal/procscan"
)

// listPlatformPorts reads lsof field output: `p<pid>` and `c<command>` head
// each process block, `n<addr>` repeats per socket (`*:3000`,
// `127.0.0.1:8080`, `[::1]:3000`). -n -P skip DNS and port-name lookups.
func listPlatformPorts(ctx context.Context) []Port {
	var ports []Port
	seen := map[[2]int]bool{} // one row per pid:port — IPv4 and IPv6 both report it
	pid, command := 0, ""
	for _, line := range strings.Split(lsof(ctx, "-nP", "-iTCP", "-sTCP:LISTEN", "-F", "pcn"), "\n") {
		if line == "" {
			continue
		}
		switch value := line[1:]; line[0] {
		case 'p':
			pid, _ = strconv.Atoi(value)
			command = ""
		case 'c':
			command = value
		case 'n':
			port, _ := strconv.Atoi(value[strings.LastIndex(value, ":")+1:])
			if port == 0 || seen[[2]int{pid, port}] {
				continue
			}
			seen[[2]int{pid, port}] = true
			ports = append(ports, Port{Port: port, PID: pid, Command: command})
		}
	}
	pids := make([]int, 0, len(ports))
	for _, p := range ports {
		if p.PID > 0 {
			pids = append(pids, p.PID)
		}
	}
	cwds := procscan.Cwds(ctx, pids)
	for i := range ports {
		ports[i].Cwd = cwds[ports[i].PID]
	}
	return ports
}

// lsof returns whatever stdout it produced. Exit 1 means "no matches" and still
// carries partial output; a missing binary yields "".
func lsof(ctx context.Context, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	_ = cmd.Run()
	return stdout.String()
}
