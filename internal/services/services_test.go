package services

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogo/host/internal/terminal"
	"github.com/repogo/host/internal/testwait"
)

func TestParseKeepsV1Rules(t *testing.T) {
	config := parseEnvironmentConfig([]byte(`{
		"future": true,
		"services": [
			{"name": "Web App", "cmd": "npm run dev", "port": 3000, "expose": true,
			 "readyWhen": {"port": 3000, "timeoutMs": 500}, "idleStop": "never", "unknown": 1},
			{"name": "install", "type": "setup", "cmd": "npm ci", "autoStart": false},
			{"name": "api", "cmd": "go run .", "target": "api.localhost:8080", "readyWhen": {}},
			{"name": "no-cmd"},
			{"name": "!!", "cmd": "x"},
			{"name": "web-app", "cmd": "duplicate"},
			{"name": "odd", "type": "daemon", "cmd": "x", "target": "  "}
		]
	}`))
	if config == nil {
		t.Fatal("nil config")
	}
	var names []string
	for _, s := range config {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "web-app,install,api,odd" {
		t.Fatalf("services = %s", got)
	}
	web, install, api, odd := config[0], config[1], config[2], config[3]
	if web.Target != "3000" || !web.Expose || web.Ready == nil || web.Ready.Port != 3000 || web.Ready.TimeoutMs != 500 ||
		web.IdleStop != "never" || !web.AutoStart || web.Type != "service" {
		t.Errorf("web = %+v", web)
	}
	if install.Type != "setup" || install.AutoStart {
		t.Errorf("install = %+v", install)
	}
	if api.Target != "api.localhost:8080" || api.Ready != nil {
		t.Errorf("api = %+v; an empty readyWhen is no gate", api)
	}
	if odd.Type != "service" || odd.Target != "" {
		t.Errorf("odd = %+v", odd)
	}

	for _, raw := range []string{``, `{}`, `{"services": []}`, `{"services": [{"name": "x"}]}`, `not json`} {
		if parseEnvironmentConfig([]byte(raw)) != nil {
			t.Errorf("%q parsed as runnable", raw)
		}
	}
}

func TestEnvKeyAndTargetPort(t *testing.T) {
	if got := envKey("web-app"); got != "WEB_APP" {
		t.Errorf("envKey = %s", got)
	}
	for target, want := range map[string]int{"3000": 3000, "web.localhost:8080": 8080, "web.localhost": 0, "": 0} {
		if got := targetPort(target); got != want {
			t.Errorf("targetPort(%q) = %d, want %d", target, got, want)
		}
	}
}

func TestSubstituteOnlyExpandsEnvironmentVars(t *testing.T) {
	env := map[string]string{"ENVIRONMENT_URL_API": "https://a.repogo.dev"}
	got := substitute("${ENVIRONMENT_URL_API}/v1 $HOME $ENVIRONMENT_MISSING", env)
	if got != "https://a.repogo.dev/v1 $HOME $ENVIRONMENT_MISSING" {
		t.Fatalf("substitute = %q", got)
	}
}

// --- a runner over fake terminals --------------------------------------------

type fakeTerms struct {
	mu    sync.Mutex
	live  map[string]terminal.Spec
	order []string // managed ids, in the order they started or ran
}

func (f *fakeTerms) Start(spec terminal.Spec, _ func(int)) (terminal.Info, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.live[spec.Managed]; ok {
		return terminal.Info{SessionID: "s-" + spec.Managed}, true, nil
	}
	f.live[spec.Managed] = spec
	f.order = append(f.order, spec.Managed)
	return terminal.Info{SessionID: "s-" + spec.Managed}, false, nil
}

func (f *fakeTerms) Run(spec terminal.Spec) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, spec.Managed)
	return 0, nil
}

func (f *fakeTerms) FindManaged(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.live[id]; ok {
		return "s-" + id
	}
	return ""
}

func (f *fakeTerms) Managed(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.live {
		if strings.HasPrefix(id, prefix) {
			out = append(out, id)
		}
	}
	return out
}

func (f *fakeTerms) StopManaged(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.live[id]
	delete(f.live, id)
	return ok
}

func (f *fakeTerms) spec(root, name string) (terminal.Spec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.live[managedID(root, name)]
	return s, ok
}

func (f *fakeTerms) started() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

type anyPath struct{}

func (anyPath) Contain(path string) (string, error) { return filepath.Clean(path), nil }

type rig struct {
	m        *Manager
	terms    *fakeTerms
	root     string
	secrets  chan []string
	announce chan Status
	// asked is every start request sent; approve answers each pending one.
	asked   chan Request
	approve bool
}

func newRig(t *testing.T, manifest string) *rig {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, configName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &rig{
		terms: &fakeTerms{live: map[string]terminal.Spec{}}, root: root,
		secrets: make(chan []string, 4), announce: make(chan Status, 64),
		asked: make(chan Request, 16), approve: true,
	}
	r.m = New(t.Context(), Deps{
		Terminals: r.terms,
		Paths:     anyPath{},
		URLs:      func() map[int]string { return map[int]string{4000: "https://abc.repogo.dev"} },
		Secrets: func(_ context.Context, _ string, handles, services []string) map[string]map[string]string {
			r.secrets <- append(append([]string(nil), handles...), services...)
			return map[string]map[string]string{"my-secrets": {"SECRET": "from-file", "TOKEN": "t"}}
		},
		Announce: func(s Status) {
			select {
			case r.announce <- s:
			default:
			}
		},
		Ask: func(req Request) int {
			r.asked <- req
			if req.State == StatePending && r.approve {
				go r.m.Answer(req.RequestID, true)
			}
			return 1
		},
		Label: "Studio",
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return r
}

// idle is true once no start is in flight for the rig's root.
func (r *rig) idle() bool {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	st, ok := r.m.states[r.root]
	return ok && !st.starting
}

// running is true once every named service is up and the start that launched
// them has finished, so the next start is not refused as a duplicate.
func (r *rig) running(names ...string) func() bool {
	return func() bool {
		for _, name := range names {
			if _, ok := r.terms.spec(r.root, name); !ok {
				return false
			}
		}
		return r.idle()
	}
}

func TestGraphOrderAndPrecedence(t *testing.T) {
	r := newRig(t, `{"services": [
		{"name": "web", "cmd": "next dev", "dependsOn": ["api"], "envFrom": ["My Secrets"],
		 "env": {"SECRET": "inline", "API": "${ENVIRONMENT_URL_API}/v1"}},
		{"name": "api", "cmd": "go run .", "target": 4000, "expose": true, "dependsOn": ["install"]},
		{"name": "install", "type": "setup", "cmd": "npm ci"},
		{"name": "escape", "cmd": "x", "cwd": "../elsewhere"}
	]}`)
	r.m.Boot(context.Background(), r.root)
	testwait.For(t, "web", r.running("web", "api"))

	order := r.terms.started()
	want := []string{managedID(r.root, "install"), managedID(r.root, "api"), managedID(r.root, "web")}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	web, _ := r.terms.spec(r.root, "web")
	for key, value := range map[string]string{
		"SECRET":                 "inline", // inline env beats envFrom
		"TOKEN":                  "t",
		"API":                    "https://abc.repogo.dev/v1",
		"ENVIRONMENT_TARGET_API": "4000",
		"ENVIRONMENT_URL_API":    "https://abc.repogo.dev",
		"ENVIRONMENT_DOMAIN_API": "abc.repogo.dev",
	} {
		if web.Env[key] != value {
			t.Errorf("web %s = %q, want %q", key, web.Env[key], value)
		}
	}
	if web.Dir != r.root || web.Project != r.root {
		t.Errorf("web runs in %s under %s", web.Dir, web.Project)
	}
	if got := <-r.secrets; strings.Join(got, ",") != "my-secrets,web" {
		t.Errorf("secrets asked for %v, want one batch of my-secrets for web", got)
	}
	if _, ok := r.terms.spec(r.root, "escape"); ok {
		t.Error("a service whose cwd escapes the repo was started")
	}

	st := r.status(t, filepath.Join(r.root, "src"))
	if st.Root != r.root || len(st.Services) != 4 {
		t.Fatalf("status = %+v", st)
	}
	api := st.Services[1]
	if !api.Running || api.SessionID == "" || api.URL != "https://abc.repogo.dev" {
		t.Errorf("api = %+v", api)
	}
}

func TestBootHonoursTheOptOut(t *testing.T) {
	t.Setenv(OptOutEnv, "false")
	r := newRig(t, `{"services": [{"name": "web", "cmd": "x"}]}`)
	r.m.Boot(context.Background(), r.root)
	// A start marks its root starting before it returns; the opt-out never gets there.
	r.m.mu.Lock()
	_, started := r.m.states[r.root]
	r.m.mu.Unlock()
	if started {
		t.Fatal("started with the opt-out set")
	}
}

// status is the path's services as its services.status push reports it.
func (r *rig) status(t *testing.T, path string) Status {
	t.Helper()
	root, err := r.m.root(path)
	if err != nil {
		t.Fatal(err)
	}
	return r.m.status(root)
}

func (r *rig) deadline() time.Time {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if st, ok := r.m.states[r.root]; ok {
		return st.deadline
	}
	return time.Time{}
}

// The demand path: a device opening the project (projectwatch's first watcher)
// starts it, the last one leaving arms the countdown, and expiry stops all
// but idleStop never.
func TestDemandStartsAndIdleCountdownStops(t *testing.T) {
	r := newRig(t, `{"services": [
		{"name": "web", "cmd": "x"},
		{"name": "worker", "cmd": "x", "idleStop": "now"},
		{"name": "db", "cmd": "x", "idleStop": "never"}
	]}`)
	sub := filepath.Join(r.root, "app")

	r.m.Active(sub, true)
	testwait.For(t, "services", r.running("web", "worker", "db"))

	r.m.Active(sub, false)
	if _, ok := r.terms.spec(r.root, "worker"); ok {
		t.Error("idleStop now survived the last watcher leaving")
	}
	deadline := r.deadline()
	if deadline.IsZero() {
		t.Fatal("no countdown armed")
	}
	if st := r.status(t, r.root); st.IdleDeadline == "" {
		t.Error("status does not carry the countdown")
	}

	// Demand returning cancels it; web is still up, so nothing restarts.
	r.m.Active(sub, true)
	if !r.deadline().IsZero() {
		t.Fatal("countdown still armed with a watcher back")
	}
	r.m.expire(r.root, deadline)
	if _, ok := r.terms.spec(r.root, "web"); !ok {
		t.Fatal("a stale countdown stopped a watched project")
	}

	r.m.Active(sub, false)
	r.m.expire(r.root, r.deadline())
	if _, ok := r.terms.spec(r.root, "web"); ok {
		t.Error("web survived the countdown")
	}
	if _, ok := r.terms.spec(r.root, "db"); !ok {
		t.Error("idleStop never was stopped by the countdown")
	}
}

// The phone's chat and tabs can hold two paths of one project: the countdown
// waits for both, and a repeated close changes nothing.
func TestDemandCountsEachPathOnce(t *testing.T) {
	r := newRig(t, `{"services": [{"name": "web", "cmd": "x"}]}`)
	sub := filepath.Join(r.root, "app")

	r.m.Active(r.root, true)
	r.m.Active(sub, true)
	testwait.For(t, "services", r.running("web"))

	r.m.Active(r.root, false)
	r.m.Active(r.root, false)
	if !r.deadline().IsZero() {
		t.Fatal("countdown armed while the other path is still open")
	}
	r.m.Active(sub, false)
	if r.deadline().IsZero() {
		t.Fatal("no countdown after the last path closed")
	}
}

func TestAPathWithoutEnvironmentJSONIsIgnored(t *testing.T) {
	r := newRig(t, `{"services": [{"name": "web", "cmd": "x"}]}`)
	other := t.TempDir()
	r.m.Active(other, true)
	r.m.Active(other, false)
	if got := r.terms.started(); len(got) != 0 {
		t.Fatalf("started %v for a project with no environment.json", got)
	}
}

// A start asks every device first: the request names the services that would
// launch, and a deny launches none of them and tells the devices it is done.
func TestStartWaitsForApproval(t *testing.T) {
	r := newRig(t, `{"services": [
		{"name": "web", "cmd": "x"},
		{"name": "later", "cmd": "x", "autoStart": false}
	]}`)
	r.approve = false
	r.m.Boot(context.Background(), r.root)

	req := <-r.asked
	if req.State != StatePending || req.HostLabel != "Studio" || req.Path != r.root ||
		strings.Join(req.Services, ",") != "web" {
		t.Fatalf("request = %+v", req)
	}
	if pending := r.m.Pending(); len(pending) != 1 || pending[0].RequestID != req.RequestID {
		t.Fatalf("pending = %+v", pending)
	}
	if got := r.terms.started(); len(got) != 0 {
		t.Fatalf("started %v before an answer", got)
	}

	if err := r.m.Answer(req.RequestID, false); err != nil {
		t.Fatal(err)
	}
	if done := <-r.asked; done.RequestID != req.RequestID || done.State != StateDone {
		t.Fatalf("after the answer = %+v", done)
	}
	testwait.For(t, "the start to end", r.idle)
	if got := r.terms.started(); len(got) != 0 {
		t.Fatalf("started %v after a deny", got)
	}
	if err := r.m.Answer(req.RequestID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second answer = %v, want ErrNotFound", err)
	}
}

func TestUnansweredStartLapses(t *testing.T) {
	r := newRig(t, `{"services": [{"name": "web", "cmd": "x"}]}`)
	r.approve = false
	r.m.approvalTimeout = time.Millisecond
	r.m.Boot(context.Background(), r.root)

	req := <-r.asked
	if done := <-r.asked; done.RequestID != req.RequestID || done.State != StateDone {
		t.Fatalf("after the timeout = %+v", done)
	}
	testwait.For(t, "the start to end", r.idle)
	if got := r.terms.started(); len(got) != 0 {
		t.Fatalf("started %v with no answer", got)
	}
	if pending := r.m.Pending(); len(pending) != 0 {
		t.Fatalf("pending = %+v", pending)
	}
}
