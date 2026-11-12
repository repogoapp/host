//go:build unix

package terminal

import (
	"errors"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// startConsole runs cmd on a new pseudo-terminal of cols by rows. pty starts
// it in a new session, so its pid is its process group.
func startConsole(cmd *exec.Cmd, cols, rows int) (console, shellProcess, error) {
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, nil, err
	}
	return ptyConsole{f}, cmdProcess{cmd}, nil
}

type ptyConsole struct{ *os.File }

func (c ptyConsole) Resize(cols, rows int) error {
	return pty.Setsize(c.File, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

type cmdProcess struct{ cmd *exec.Cmd }

func (p cmdProcess) Pid() int { return p.cmd.Process.Pid }

// Wait reaps the shell rather than reading its end off the pty: the pty
// closing and the child exiting are different events, and only Wait knows
// the code. It also keeps the shell from staying a zombie.
func (p cmdProcess) Wait() int {
	err := p.cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	return -1
}
