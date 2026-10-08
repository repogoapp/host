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

// Command is a command the host runs in a tab of its own.
type Command struct {
	Dir string // the project; its tab strip lists the tab
	Cmd string
}

// StartCommand opens a tab running c and calls ended once with the command's
// exit code, or with finished false when the tab closed first.
func (m *Manager) StartCommand(c Command, ended func(code int, finished bool)) (Info, error) {
	dir, err := m.paths.Contain(c.Dir)
	if err != nil {
		return Info{}, err
	}
	if m.Open() >= maxSessions {
		return Info{}, ErrTooMany
	}
	r, w, err := os.Pipe()
	if err != nil {
		return Info{}, err
	}

	shell := LoginShell()
	cmd := exec.Command("/bin/sh", "-c", commandScript, "sh", shell, c.Cmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	cmd.ExtraFiles = []*os.File{w}

	s := newSession(dir, shell, cmd, 80, 24)
	err = m.start(s)
	w.Close()
	if err != nil {
		r.Close()
		return Info{}, err
	}
	go func() {
		raw, _ := io.ReadAll(r)
		r.Close()
		code, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		ended(code, err == nil)
	}()
	m.log.Info("terminal: command opened", "session", s.id, "cwd", dir)
	return s.info(), nil
}
