//go:build unix

package proctree

import (
	"os/exec"
	"syscall"
)

func own(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signal sends sig to the process group: pid heads it, so -pid names it.
func signal(pid int, sig syscall.Signal) error { return syscall.Kill(-pid, sig) }
