//go:build !windows

package terminal

import (
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// commandScript runs a command in the login shell, writes its exit code to
// fd 3 and leaves the tab to an interactive shell. `trap : INT` lets Ctrl-C
// end the command but not this wrapper, as a trapped signal resets on exec.
const commandScript = `trap : INT; "$1" -l -c "$2" 3>&-; printf %d "$?" >&3; exec 3>&-; exec "$1" -l`

// commandProcess is a tab's process running line, and how its code comes
// back: through a pipe on fd 3, read to its end.
func commandProcess(shell, line string) (*exec.Cmd, *exitCode, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command("/bin/sh", "-c", commandScript, "sh", shell, line)
	cmd.ExtraFiles = []*os.File{w}
	return cmd, &exitCode{
		started: func() { w.Close() },
		wait: func(func() bool) (int, bool) {
			raw, _ := io.ReadAll(r)
			r.Close()
			code, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			return code, err == nil
		},
		abort: func() { r.Close() },
	}, nil
}
