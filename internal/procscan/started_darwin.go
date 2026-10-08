package procscan

import "golang.org/x/sys/unix"

// Started is when the process with pid began, in ms since the epoch, and
// false when there is none. With the pid it names one process, since pids
// are reused.
func Started(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k.Proc.P_pid != int32(pid) {
		return 0, false
	}
	t := k.Proc.P_starttime
	return t.Sec*1000 + int64(t.Usec)/1000, true
}
