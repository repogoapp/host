package services

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/repogo/host/internal/git"
)

// How long a project's services keep running after the last device leaves it.
const countdown = 3 * time.Minute

type state struct {
	// path is the last form a device named this root by, echoed in pushes.
	path string
	// demand is the active paths under this root, each counted once however
	// many devices have it open (projectwatch tells only first and last).
	demand   map[string]bool
	starting bool
	deadline time.Time // zero = no countdown armed
	revision int64
	timer    *time.Timer
}

// Callers hold m.mu.
func (m *Manager) stateLocked(root, path string) *state {
	st, ok := m.states[root]
	if !ok {
		st = &state{demand: map[string]bool{}}
		m.states[root] = st
	}
	if path != "" {
		st.path = path
	}
	return st
}

// root maps a path a device named to its project root: the nearest folder at
// or above it holding environment.json or .git.
func (m *Manager) root(path string) (string, error) {
	dir, err := m.deps.Paths.Contain(path)
	if err != nil {
		return "", err
	}
	top := git.Root(dir)
	for d := dir; d != top; {
		if _, err := os.Lstat(filepath.Join(d, configName)); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir, nil
		}
		d = parent
	}
	return top, nil
}

// Active is projectwatch's OnActive: a path became active on its first device or
// lost its last. A release goes to the root the path was opened under, which
// environment.json appearing since may have moved.
func (m *Manager) Active(path string, active bool) {
	if !active {
		m.released(path)
		return
	}
	root, err := m.root(path)
	if err != nil {
		return
	}
	m.demanded(root, path)
}

func (m *Manager) demanded(root, path string) {
	m.mu.Lock()
	if old, ok := m.opened[path]; ok && old != root {
		delete(m.states[old].demand, path)
	}
	m.opened[path] = root
	st := m.stateLocked(root, path)
	st.demand[path] = true
	first := len(st.demand) == 1
	cancelled := !st.deadline.IsZero()
	m.disarmLocked(st)
	demand := len(st.demand)
	m.mu.Unlock()
	m.deps.Log.Info("services: demand", "root", root, "path", path, "open", true, "demand", demand)
	if cancelled {
		m.deps.Log.Info("services: countdown cancelled, demand returned", "root", root)
	}
	if first {
		m.ensureUp(m.ctx, root)
	}
	if cancelled {
		m.bumpAndPush(root)
	}
}

func (m *Manager) released(path string) {
	m.mu.Lock()
	root, ok := m.opened[path]
	delete(m.opened, path)
	st := m.states[root]
	if !ok || st == nil || !st.demand[path] {
		m.mu.Unlock()
		return
	}
	delete(st.demand, path)
	demand := len(st.demand)
	m.mu.Unlock()
	m.deps.Log.Info("services: demand", "root", root, "path", path, "open", false, "demand", demand)
	if demand == 0 {
		m.armCountdown(root)
	}
}

// ensureUp relaunches the root's graph when nothing of it is running; any
// live service leaves it untouched.
func (m *Manager) ensureUp(ctx context.Context, root string) {
	services := m.declaredServices(root)
	if len(services) == 0 {
		return
	}
	for _, service := range services {
		if service.Running {
			return
		}
	}
	m.deps.Log.Info("services: starting on demand", "root", root)
	m.startRepo(ctx, root)
}

// armCountdown at zero demand: stop `idleStop: "now"` services immediately,
// then arm the grace countdown for whatever expiry would still have to stop.
func (m *Manager) armCountdown(root string) {
	now, never := m.idleStopNames(root)
	eager := map[string]bool{}
	remaining := 0
	for name := range m.startedServiceNames(root) {
		switch {
		case now[name]:
			eager[name] = true
		case never[name]:
		default:
			remaining++
		}
	}
	if len(eager) > 0 {
		m.deps.Log.Info("services: idleStop=now, stopping without grace", "root", root, "services", len(eager))
		m.stopManaged(root, eager)
	}
	if remaining == 0 {
		if len(eager) > 0 {
			m.bumpAndPush(root)
		}
		return
	}
	m.mu.Lock()
	st := m.stateLocked(root, "")
	deadline := time.Now().Add(countdown)
	m.disarmLocked(st)
	st.deadline = deadline
	st.timer = time.AfterFunc(countdown, func() { m.expire(root, deadline) })
	m.mu.Unlock()
	m.deps.Log.Info("services: countdown armed", "root", root, "deadline", deadline)
	m.bumpAndPush(root)
}

func (m *Manager) expire(root string, deadline time.Time) {
	m.mu.Lock()
	st, ok := m.states[root]
	// Stale-timer guard: only stop if this deadline is still the armed one and
	// demand is still zero.
	if !ok || !st.deadline.Equal(deadline) || len(st.demand) > 0 {
		m.mu.Unlock()
		return
	}
	st.deadline = time.Time{}
	st.timer = nil
	m.mu.Unlock()
	// Recomputed here, not at arm time: the manifest may have been edited and
	// services may have exited while the clock ran.
	_, never := m.idleStopNames(root)
	victims := map[string]bool{}
	for name := range m.startedServiceNames(root) {
		if !never[name] {
			victims[name] = true
		}
	}
	m.deps.Log.Info("services: countdown expired, stopping services", "root", root, "stopping", len(victims))
	m.stopManaged(root, victims)
	m.bumpAndPush(root)
}

// Callers hold m.mu.
func (m *Manager) disarmLocked(st *state) {
	st.deadline = time.Time{}
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
}

// idleStopNames: services whose `idleStop` skips the grace period ("now" stops
// at zero demand, "never" only on stop_all). Anything else is the default, so a
// typo costs the override, never the service.
func (m *Manager) idleStopNames(root string) (now, never map[string]bool) {
	now, never = map[string]bool{}, map[string]bool{}
	for _, service := range readConfig(root) {
		switch service.IdleStop {
		case "now":
			now[service.Name] = true
		case "never":
			never[service.Name] = true
		}
	}
	return now, never
}

// status is the one snapshot every response carries and every push sends.
func (m *Manager) status(root string) Status {
	out := Status{Path: root, Root: root}
	m.mu.Lock()
	if st, ok := m.states[root]; ok {
		out.Revision = st.revision
		if st.path != "" {
			out.Path = st.path
		}
		if !st.deadline.IsZero() {
			out.IdleDeadline = st.deadline.UTC().Format(time.RFC3339)
		}
	}
	m.mu.Unlock()
	out.Services = append([]Service{}, m.declaredServices(root)...)
	return out
}

// bumpAndPush bumps the revision and sends the fresh snapshot to every device.
func (m *Manager) bumpAndPush(root string) {
	m.mu.Lock()
	m.stateLocked(root, "").revision++
	m.mu.Unlock()
	m.deps.Announce(m.status(root))
}

// declaredServices is the root's environment.json with `running` read off the
// live managed sessions.
func (m *Manager) declaredServices(root string) []Service {
	out := []Service{}
	services := readConfig(root)
	urls := m.exposedURLs(services)
	for _, service := range services {
		sessionID := m.deps.Terminals.FindManaged(managedID(root, service.Name))
		out = append(out, Service{
			Name: service.Name, Type: service.Type, Cmd: service.Cmd, Target: service.Target,
			Expose: service.Expose, URL: urls[service.Target],
			Running: sessionID != "", SessionID: sessionID,
			AutoStart: service.AutoStart, IdleStop: service.IdleStop,
		})
	}
	return out
}

// startedServiceNames: the services with a live managed session under this
// root — what a teardown would actually stop, not what the manifest declares.
func (m *Manager) startedServiceNames(root string) map[string]bool {
	prefix := managedPrefix(root)
	out := map[string]bool{}
	for _, id := range m.deps.Terminals.Managed(prefix) {
		out[id[len(prefix):]] = true
	}
	return out
}

// stopManaged stops the named services' sessions under root.
func (m *Manager) stopManaged(root string, only map[string]bool) {
	for name := range only {
		m.deps.Terminals.StopManaged(managedID(root, name))
	}
}

func managedID(root, name string) string { return "services:" + root + ":" + name }
func managedPrefix(root string) string   { return "services:" + root + ":" }
