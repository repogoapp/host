package terminal

import (
	"os"
	"os/exec"
	"strings"
)

// Managed sessions are the host's own (environment.json dev servers and setup
// steps). They list under their project like any tab, so clients attach as to
// a shell; the owner finds, stops and waits on them by its own id.

// Spec is a command to run in a managed session.
type Spec struct {
	Managed string            // the owner's id; one live session per id
	Project string            // the project whose tab strip lists it
	Dir     string            // where the command runs, inside Project
	Env     map[string]string // over the host's own environment
	Cmd     string
}

// Start runs spec's command in a login shell, or returns the live session
// already running under its id. onExit is called when it ends, however it ends.
func (m *Manager) Start(spec Spec, onExit func(code int)) (info Info, running bool, err error) {
	s, running, err := m.startManaged(spec, onExit)
	if err != nil {
		return Info{}, false, err
	}
	return s.info(), running, nil
}

// Run is Start that waits for the command to finish and returns its exit code;
// a session already running under the id is waited on instead.
func (m *Manager) Run(spec Spec) (int, error) {
	s, _, err := m.startManaged(spec, nil)
	if err != nil {
		return -1, err
	}
	<-s.ended
	return s.code, nil
}

func (m *Manager) startManaged(spec Spec, onExit func(int)) (*session, bool, error) {
	m.mu.Lock()
	if s := m.managedLocked(spec.Managed); s != nil {
		m.mu.Unlock()
		return s, true, nil
	}
	m.mu.Unlock()

	// A login shell for the same PATH the user's own terminal has; -c so the
	// session ends with the command and a crashed server reads as stopped.
	shell := LoginShell()
	cmd := exec.Command(shell, "-l", "-c", spec.Cmd)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	for key, value := range spec.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}

	s := newSession(spec.Project, shell, cmd, 80, 24)
	s.managed, s.onExit = spec.Managed, onExit
	if err := m.start(s); err != nil {
		return nil, false, err
	}
	m.log.Info("terminal: managed session opened", "session", s.id, "managed", spec.Managed, "dir", spec.Dir)
	return s, false, nil
}

// FindManaged is the session id running under a managed id, "" when none.
func (m *Manager) FindManaged(managed string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.managedLocked(managed); s != nil {
		return s.id
	}
	return ""
}

// Managed lists the managed ids of live sessions under a prefix.
func (m *Manager) Managed(prefix string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, s := range m.sessions {
		if s.managed != "" && !s.stopping && strings.HasPrefix(s.managed, prefix) {
			out = append(out, s.managed)
		}
	}
	return out
}

// StopManaged kills the session running under a managed id; false when none is.
func (m *Manager) StopManaged(managed string) bool {
	return m.stop(func(id string) bool { return id == managed }) > 0
}

// stop hides the victims from lookups at once, so a status read right after
// reports them stopped, then kills them the way Close does.
func (m *Manager) stop(match func(managed string) bool) int {
	m.mu.Lock()
	var victims []*session
	for _, s := range m.sessions {
		if s.managed != "" && !s.stopping && match(s.managed) {
			s.stopping = true
			victims = append(victims, s)
		}
	}
	m.mu.Unlock()
	for _, s := range victims {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.pty.Close()
	}
	return len(victims)
}

func (m *Manager) managedLocked(managed string) *session {
	if managed == "" {
		return nil
	}
	for _, s := range m.sessions {
		if s.managed == managed && !s.stopping {
			return s
		}
	}
	return nil
}
