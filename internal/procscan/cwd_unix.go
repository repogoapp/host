//go:build !windows

package procscan

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Cwds resolves each pid's working directory: /proc on Linux, one batched lsof
// call elsewhere because lsof per pid is slow. A pid we cannot read is absent.
func Cwds(ctx context.Context, pids []int) map[int]string {
	out := make(map[int]string, len(pids))
	if len(pids) == 0 {
		return out
	}
	if runtime.GOOS == "linux" {
		for _, pid := range pids {
			if target, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd"); err == nil {
				out[pid] = target
			}
		}
		return out
	}

	ids := make([]string, len(pids))
	for i, pid := range pids {
		ids[i] = strconv.Itoa(pid)
	}
	ctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	// -d cwd limits to the working-directory descriptor; -Fn is the field format.
	// lsof exits non-zero when any pid has vanished, having still reported the rest.
	raw, _ := exec.CommandContext(ctx, "lsof", "-a", "-p", strings.Join(ids, ","), "-d", "cwd", "-Fn").Output()

	// Records: "p<pid>" starts one, "n<path>" is the directory of the latest pid.
	pid := 0
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
		case 'n':
			if pid > 0 && out[pid] == "" {
				out[pid] = line[1:]
			}
		}
	}
	return out
}
