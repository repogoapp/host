package terminal

// Command is a command the host runs in a tab of its own.
type Command struct {
	Dir string // the project; its tab strip lists the tab
	Cmd string
	Env map[string]string // over the host's own environment
}

// exitCode is how a tab's command reports its code: started runs once the
// tab has started or failed to, wait blocks until the code comes (false when
// the tab closed first; open says whether it still is), and abort undoes
// everything when the tab never started. env is what the command needs on
// top of the host's environment.
type exitCode struct {
	env     []string
	started func()
	wait    func(open func() bool) (int, bool)
	abort   func()
}

// StartCommand opens a tab running c and calls ended once with the command's
// exit code, or with finished false when the tab closed first. The pid is the
// process group a stop signals.
func (m *Manager) StartCommand(c Command, ended func(code int, finished bool)) (sessionID string, pid int, err error) {
	dir, err := m.paths.Contain(c.Dir)
	if err != nil {
		return "", 0, err
	}
	if m.Open() >= maxSessions {
		return "", 0, ErrTooMany
	}
	shell := LoginShell()
	cmd, exit, err := commandProcess(shell, c.Cmd)
	if err != nil {
		return "", 0, err
	}
	cmd.Dir = dir
	cmd.Env = append(append(Environ(c.Env), "TERM=xterm-256color", "COLORTERM=truecolor"), exit.env...)

	s := newSession(dir, shell, cmd, 80, 24)
	err = m.start(s)
	exit.started()
	if err != nil {
		exit.abort()
		return "", 0, err
	}
	go func() {
		code, ok := exit.wait(s.open)
		ended(code, ok)
	}()
	m.log.Info("terminal: command opened", "session", s.id, "cwd", dir)
	return s.id, s.proc.Pid(), nil
}
