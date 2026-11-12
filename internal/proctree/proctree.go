// Package proctree runs a command as the head of its own process tree and
// stops the whole tree, so a CLI's children (a launcher's real process, a
// tool it ran) never outlive it or keep its pipes and ports open.
//
// On Unix the tree is a process group, signalled as one. Windows has no
// signals to send a console tree, so every stop there ends the tree at once.
package proctree

import (
	"os/exec"
	"syscall"
)

// Own makes cmd the head of its own tree when it starts. A cmd made with
// exec.CommandContext that should end its tree when cancelled also takes
// KillOnCancel.
func Own(cmd *exec.Cmd) { own(cmd) }

// KillOnCancel has a cancelled cmd's context kill its whole tree rather than
// only cmd. cmd must come from exec.CommandContext.
func KillOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return Kill(cmd.Process.Pid) }
}

// Kill ends the tree headed by pid at once.
func Kill(pid int) error { return Signal(pid, syscall.SIGKILL) }

// Signal sends sig to the tree headed by pid: SIGTERM or SIGHUP asks it to
// stop where the system can ask, SIGKILL ends it.
func Signal(pid int, sig syscall.Signal) error { return signal(pid, sig) }
