//go:build linux

package ports

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/repogo/host/internal/procscan"
)

// listPlatformPorts reads /proc/net/tcp{,6} for LISTEN sockets, which sees
// listeners lsof drops, then finds each socket's owner through /proc/<pid>/fd.
// A socket whose owner we cannot read is still reported, with pid 0.
func listPlatformPorts(ctx context.Context) []Port {
	inodeToPort := map[string]int{}
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			continue // tcp6 is absent when IPv6 is disabled
		}
		lines := strings.Split(string(raw), "\n")
		for _, line := range lines[1:] {
			cols := strings.Fields(line)
			// sl local_address rem_address st ... inode → local=1, state=3, inode=9
			if len(cols) < 10 || cols[3] != "0A" {
				continue
			}
			local := cols[1]
			port, err := strconv.ParseInt(local[strings.LastIndex(local, ":")+1:], 16, 32)
			inode := cols[9]
			if err != nil || port == 0 || inode == "" || inode == "0" {
				continue
			}
			inodeToPort[inode] = int(port)
		}
	}
	if len(inodeToPort) == 0 {
		return nil
	}

	inodeToPid := map[string]int{}
	procDirs, _ := os.ReadDir("/proc")
	for _, entry := range procDirs {
		if len(inodeToPid) == len(inodeToPort) {
			break // all attributed
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == 0 {
			continue
		}
		fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err != nil {
			continue // not ours, or gone
		}
		for _, fd := range fds {
			link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
			if err != nil {
				continue
			}
			inode, ok := strings.CutPrefix(link, "socket:[")
			if !ok || !strings.HasSuffix(inode, "]") {
				continue
			}
			inode = strings.TrimSuffix(inode, "]")
			if _, wanted := inodeToPort[inode]; wanted {
				if _, has := inodeToPid[inode]; !has {
					inodeToPid[inode] = pid
				}
			}
		}
	}

	pids := make([]int, 0, len(inodeToPid))
	for _, pid := range inodeToPid {
		pids = append(pids, pid)
	}
	cwds := procscan.Cwds(ctx, pids)

	ports := make([]Port, 0, len(inodeToPort))
	seen := map[[2]int]bool{} // one row per pid:port — IPv4 and IPv6 both report it
	commByPid := map[int]string{}
	for inode, port := range inodeToPort {
		pid := inodeToPid[inode]
		if seen[[2]int{pid, port}] {
			continue
		}
		seen[[2]int{pid, port}] = true
		if _, ok := commByPid[pid]; !ok && pid > 0 {
			raw, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
			commByPid[pid] = strings.TrimSpace(string(raw))
		}
		ports = append(ports, Port{Port: port, PID: pid, Command: commByPid[pid], Cwd: cwds[pid]})
	}
	return ports
}
