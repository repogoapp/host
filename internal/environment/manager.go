package environment

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/envsource"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/terminal"
)

const (
	configName       = "environment.json"
	depWaitTimeout   = 5 * time.Minute
	defaultReadyWait = 2 * time.Minute

	// OptOutEnv set to "false" keeps boot and post-clone starts off, as in v1.
	OptOutEnv = "REPOGO_RUN_ENVIRONMENT_JSON"
)

// Terminals is the terminal manager's managed-session half.
type Terminals interface {
	Start(spec terminal.Spec, onExit func(code int)) (terminal.Info, bool, error)
	Run(spec terminal.Spec) (int, error)
	FindManaged(managed string) string
	Managed(prefix string) []string
	StopManaged(managed string) bool
}

type Deps struct {
	Terminals Terminals
	// Paths contains a path a device names, as every project method does.
	Paths files.Container
	// URLs is the public URL open for each local port, for `expose`.
	URLs func() map[int]string
	// Secrets resolves one start's envFrom handles to their variables.
	Secrets func(ctx context.Context, root string, handles, services []string) map[string]map[string]string
	// Announce sends an environment.status to every device.
	Announce func(Status)
	Log      *slog.Logger
}

type Manager struct {
	deps Deps
	// ctx is the host's: starts a device's demand triggers outlive its call.
	ctx context.Context
	mu  sync.Mutex
	// Project root → lifecycle state.
	states map[string]*state
	// opened is each active path's root, as resolved when it was opened.
	opened map[string]string
}

func New(ctx context.Context, deps Deps) *Manager {
	return &Manager{deps: deps, ctx: ctx, states: map[string]*state{}, opened: map[string]string{}}
}

// Boot starts each root that has an environment.json: the clones this host
// made, at serve start, and a fresh clone. Worktrees are never passed here:
// a repo's worktrees declare the same ports and would collide on bind.
func (m *Manager) Boot(ctx context.Context, roots ...string) {
	if os.Getenv(OptOutEnv) == "false" {
		return
	}
	for _, root := range roots {
		if _, err := os.Stat(filepath.Join(root, configName)); err == nil {
			m.deps.Log.Info("environment: starting", "root", root)
			m.startRepo(ctx, root)
		}
	}
}

func readConfig(root string) []serviceConfig {
	raw, err := os.ReadFile(filepath.Join(root, configName))
	if err != nil {
		return nil
	}
	return parseEnvironmentConfig(raw)
}

// startRepo launches one repo's graph gated by dependsOn (setup deps on exit,
// services on spawn or readyWhen). envFrom secrets are fetched once, on first
// need, so a start asks for one approval. Returns at once.
func (m *Manager) startRepo(ctx context.Context, root string) {
	services := readConfig(root)
	if services == nil {
		m.deps.Log.Warn("environment: invalid or empty environment.json", "root", root)
		return
	}
	m.mu.Lock()
	st := m.stateLocked(root, "")
	if st.starting {
		m.mu.Unlock()
		return
	}
	st.starting = true
	m.mu.Unlock()

	envMap := buildEnvMap(services, m.exposedURLs(services))

	handles := map[string]bool{}
	var needing []string
	for _, service := range services {
		for _, handle := range service.EnvFrom {
			handles[envsource.NormalizeHandle(handle)] = true
		}
		if len(service.EnvFrom) > 0 {
			needing = append(needing, service.Name)
		}
	}
	var secretsOnce sync.Once
	var secrets map[string]map[string]string
	fetchSecrets := func() map[string]map[string]string {
		secretsOnce.Do(func() { secrets = m.deps.Secrets(ctx, root, keys(handles), needing) })
		return secrets
	}

	satisfied := map[string]chan struct{}{}
	for _, service := range services {
		satisfied[service.Name] = make(chan struct{})
	}
	var wg sync.WaitGroup
	for _, service := range services {
		wg.Add(1)
		go func(service serviceConfig) {
			defer wg.Done()
			// Satisfied even on failure, so dependents proceed.
			defer close(satisfied[service.Name])
			m.launchOne(ctx, service, root, envMap, satisfied, fetchSecrets)
		}(service)
	}
	go func() {
		wg.Wait()
		m.mu.Lock()
		st.starting = false
		m.mu.Unlock()
	}()
}

func (m *Manager) launchOne(
	ctx context.Context,
	service serviceConfig,
	root string,
	envMap map[string]string,
	satisfied map[string]chan struct{},
	fetchSecrets func() map[string]map[string]string,
) {
	for _, dep := range service.DependsOn {
		gate, ok := satisfied[envsource.NormalizeHandle(dep)]
		if !ok {
			continue
		}
		select {
		case <-gate:
		case <-time.After(depWaitTimeout):
			// A cycle or a stuck dep must never wedge the graph forever.
			m.deps.Log.Warn("environment: dep wait timed out; starting anyway", "service", service.Name, "dep", dep)
		case <-ctx.Done():
			return
		}
	}
	if !service.AutoStart {
		return // defined but not auto-started; dependents proceed
	}

	serviceEnv := map[string]string{}
	if len(service.EnvFrom) > 0 {
		byHandle := fetchSecrets()
		for _, handle := range service.EnvFrom {
			for key, value := range byHandle[envsource.NormalizeHandle(handle)] {
				serviceEnv[key] = value
			}
		}
	}
	// Precedence (low → high): envFrom secrets, ENVIRONMENT_* peer vars, then
	// the service's own inline env (which may reference ${ENVIRONMENT_*}).
	for key, value := range envMap {
		serviceEnv[key] = value
	}
	for key, value := range service.Env {
		serviceEnv[key] = substitute(value, envMap)
	}

	dir := root
	if service.Cwd != "" {
		dir = filepath.Join(root, service.Cwd)
		if !files.Within(root, dir) {
			m.deps.Log.Warn("environment: service cwd escapes repo root, skipping", "service", service.Name)
			return
		}
	}
	spec := terminal.Spec{Managed: managedID(root, service.Name), Project: root, Dir: dir, Env: serviceEnv, Cmd: service.Cmd}

	if service.Type == "setup" {
		m.deps.Log.Info("environment: setup running", "service", service.Name, "dir", dir)
		code, err := m.deps.Terminals.Run(spec)
		m.deps.Log.Info("environment: setup finished", "service", service.Name, "exit", code, "err", err)
		return
	}

	info, running, err := m.deps.Terminals.Start(spec, func(int) { m.bumpAndPush(root) })
	if err != nil {
		m.deps.Log.Warn("environment: service launch failed", "service", service.Name, "err", err)
		return
	}
	if !running {
		m.bumpAndPush(root)
	}
	m.deps.Log.Info("environment: service started", "service", service.Name, "session", info.SessionID, "already_running", running)

	// A service with readyWhen holds its dependents until it's actually serving.
	if service.Ready != nil {
		m.waitForReady(ctx, service)
	}
}

// readiness

func (m *Manager) waitForReady(ctx context.Context, service serviceConfig) {
	timeout := defaultReadyWait
	if service.Ready.TimeoutMs > 0 {
		timeout = time.Duration(service.Ready.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	for !checkReady(ctx, service.Ready) {
		select {
		case <-ctx.Done():
			m.deps.Log.Warn("environment: readiness timed out; starting dependents anyway", "service", service.Name)
			return
		case <-poll.C:
		}
	}
	m.deps.Log.Info("environment: service ready", "service", service.Name)
}

func checkReady(ctx context.Context, ready *readyWhen) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if ready.Port > 0 {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(ready.Port))
		if err != nil {
			return false
		}
		conn.Close()
	}
	if ready.HTTP != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ready.HTTP, nil)
		if err != nil {
			return false
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		res.Body.Close()
		if res.StatusCode >= 500 {
			return false
		}
	}
	return true
}

// env plumbing

// exposedURLs is target → public URL for each exposed service with a tunnel
// open on its port. The host cannot mint a tunnel itself (repogo.app grants
// the slug to a phone), so an exposed service uses the one already open.
func (m *Manager) exposedURLs(services []serviceConfig) map[string]string {
	byPort := m.deps.URLs()
	out := map[string]string{}
	for _, service := range services {
		if !service.Expose || service.Target == "" {
			continue
		}
		if url := byPort[targetPort(service.Target)]; url != "" {
			out[service.Target] = url
		}
	}
	return out
}

// buildEnvMap: ENVIRONMENT_* vars keyed by service name (what environment.json
// references). TARGET for any service with one; URL/DOMAIN only when exposed
// and a public address exists.
func buildEnvMap(services []serviceConfig, opened map[string]string) map[string]string {
	env := map[string]string{}
	for _, service := range services {
		if service.Target == "" {
			continue
		}
		key := envKey(service.Name)
		env["ENVIRONMENT_TARGET_"+key] = service.Target
		if url, ok := opened[service.Target]; service.Expose && ok {
			env["ENVIRONMENT_URL_"+key] = url
			env["ENVIRONMENT_DOMAIN_"+key] = strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
		}
	}
	return env
}

// substitute expands ${ENVIRONMENT_X} / $ENVIRONMENT_X against the env map.
func substitute(value string, envMap map[string]string) string {
	return os.Expand(value, func(name string) string {
		if resolved, ok := envMap[name]; ok && strings.HasPrefix(name, "ENVIRONMENT_") {
			return resolved
		}
		return "$" + name
	})
}

func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
