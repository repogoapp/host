package procscan

import (
	"os"
	"strconv"
	"strings"
)

// Started is when the process with pid began, in clock ticks since boot, and
// false when there is none. With the pid it names one process, since pids
// are reused; only equality matters, so the unit is the kernel's own.
func Started(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// The command name is in parentheses and may hold spaces; fields follow it.
	stat := string(raw)
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(stat[end+1:])
	// starttime is field 22; fields here start at field 3.
	if len(fields) < 20 {
		return 0, false
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	return ticks, err == nil
}
