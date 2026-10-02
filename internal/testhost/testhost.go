// Package testhost builds the host through internal/host on temporary data,
// so a test drives the same wiring the host ships with and never the
// developer's own accounts or ~/.repogo.
package testhost

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/host"
	"github.com/repogo/host/internal/hostupdate"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/power"
	"github.com/repogo/host/internal/release"
	"github.com/repogo/host/internal/rpc/registry"
)

// Host is a host over temporary data. Root is a project the files service
// serves, holding readme.txt ("hello"); Config is every service it serves.
type Host struct {
	*host.Host
	Config  registry.Config
	Root    string
	Devices *device.Store
}

// New builds the host, not started, and closes it when the test ends. Agents
// never run: no provider adapter is registered, no hook is installed, and every
// provider home is under a temporary root. opts adjust the config first.
func New(t testing.TB, opts ...func(*host.Config)) *Host {
	t.Helper()
	cfg := Config(t)
	for _, opt := range opts {
		opt(&cfg)
	}
	root := filepath.Join(cfg.Projects, "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := host.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return &Host{Host: h, Config: h.Services, Root: root, Devices: h.Services.Devices}
}

// Config is a host config on fresh temporary directories with every
// process-level integration replaced by one that touches nothing real.
func Config(t testing.TB) host.Config {
	t.Helper()
	dir := func(name string) string {
		// Resolved: on a Mac /var is a symlink, and containment compares resolved paths.
		base, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(base, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	state := dir("state")
	return host.Config{
		State: state, Home: dir("home"), Projects: dir("projects"), Attachments: dir("attachments"),
		ProviderRoot: dir("providers"),
		Token:        Token, ServerID: "testhost",
		Label: "testhost", Gateway: "127.0.0.1:1", GatewayPlaintext: true,
		Getenv: func(string) string { return "" },
		Update: hostupdate.Deps{
			Version: "dev", Binary: filepath.Join(state, "repogo"), Marker: filepath.Join(state, "update.json"),
			Latest: release.Latest, Stage: release.Stage, Restart: func() {},
		},
		Agents: func(deps agent.Dependencies) (*agents.Registry, error) {
			r, err := agents.New(deps)
			if r != nil {
				r.Adapters = nil
			}
			return r, err
		},
		InstallHooks: func(*slog.Logger, []notify.Provider) {},
		Power: func(*slog.Logger, func() bool, func(power.Battery)) (host.Keeper, error) {
			return noPower{}, nil
		},
		PublicIP: func(context.Context) (string, error) { return "", nil },
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// noPower holds nothing and never reads a battery.
type noPower struct{}

func (noPower) Battery() (power.Battery, bool) { return power.Battery{}, false }
func (noPower) Run(context.Context)            {}
func (noPower) Release()                       {}
