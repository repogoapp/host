// Package host assembles the host's services and owns their lifecycle: New
// builds and wires everything, Listen admits devices, Start runs the workers,
// Close stops them in order. The repogo command and internal/testhost both
// build through New, so tests exercise the wiring production ships.
package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents"
	"github.com/repogo/host/internal/chat"
	"github.com/repogo/host/internal/chatlive"
	"github.com/repogo/host/internal/chatsync"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/hostinfo"
	"github.com/repogo/host/internal/hostlink"
	"github.com/repogo/host/internal/hostupdate"
	"github.com/repogo/host/internal/liveactivity"
	"github.com/repogo/host/internal/mcp"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/power"
	"github.com/repogo/host/internal/projectsync"
	"github.com/repogo/host/internal/projectwatch"
	"github.com/repogo/host/internal/push"
	"github.com/repogo/host/internal/repogomcp"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/rpc/registry"
	"github.com/repogo/host/internal/services"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/tunnel"
	"github.com/repogo/host/internal/wsserver"
)

// Config is where a host keeps its data and the process-level integrations
// production and tests supply differently. Every field is required except
// ProviderRoot, Port and Relay.
type Config struct {
	// State holds the host's own files: databases, identity, settings.
	State string
	// Home is the user's home: the folder picker browses it and env sources bind under it.
	Home string
	// Projects holds clones, new folders and chat worktrees.
	Projects string
	// Attachments holds the files sent with prompts; Read alone reaches them.
	Attachments string
	// ProviderRoot, when set, holds every provider's home; empty leaves each its own.
	ProviderRoot string

	// Port, Token and ServerID are the loopback server's; Port 0 picks a free one.
	Port            int
	Token, ServerID string
	// Relay, when set, is dialled so phones off this network reach the host.
	Relay string

	// Label names this host to its phones.
	Label string
	// Gateway is the tunnel gateway's host:port; Plaintext skips TLS for a local one.
	Gateway          string
	GatewayPlaintext bool
	// Getenv reads a cloud session's stop time.
	Getenv func(string) string

	// Update replaces and restarts the binary; New fills Turns, Terminals, Actions and Builds.
	Update hostupdate.Deps
	// UpdateFailed is why the last update was rolled back, for host.status.
	UpdateFailed string

	// Agents builds the providers: agents.New.
	Agents func(agent.Dependencies) (*agents.Registry, error)
	// InstallHooks wires the hook helper into each agent's config: notify.EnsureHooks.
	InstallHooks func(*slog.Logger, []notify.Provider)
	// Power keeps the machine awake while wanted reports true: power.New.
	Power func(log *slog.Logger, wanted func() bool, onBattery func(power.Battery)) (Keeper, error)
	// PublicIP looks up this machine's public address for host.status: hostinfo.LookupPublicIP.
	PublicIP func(context.Context) (string, error)

	Log *slog.Logger
}

// Keeper is the power hold: *power.Keeper, or a test's stand-in.
type Keeper interface {
	hostinfo.Battery
	Run(context.Context)
	Release()
}

// Host is one assembled host. Its exported fields are what callers and tests
// reach the services through.
type Host struct {
	// Services is every service the router serves; Router serves them.
	Services registry.Config
	Router   *rpc.Router
	// ChatDeps is what Services.Chats was built from.
	ChatDeps chat.Deps
	// Pushes routes events to devices; a transport attaches the devices it carries.
	Pushes *emit.Mux

	cfg     Config
	log     *slog.Logger
	ctx     context.Context // ends at Close; every owned worker runs on it
	cancel  context.CancelFunc
	workers sync.WaitGroup
	closers []func()
	closing sync.Once

	agents   *agents.Registry
	mcps     *mcp.Service
	mgr      *agent.Manager
	sessions *session.Store
	bridge   *notify.Bridge
	keeper   Keeper

	info    *hostinfo.Service
	devices *device.Store
	pairer  *device.Pairer
	emitter *emit.Emitter
	tunnels *tunnel.Service

	db        *store.Store
	announcer *chatlive.Announcer
	live      *chatlive.Manager
	syncer    *chatsync.Syncer
	watch     *projectwatch.Manager
	envs      *services.Manager
	clones    func() []string

	srv *wsserver.Server
	// Set in New only when there is a relay; APNs goes through it.
	link       *hostlink.Link
	alerts     *push.Notifier
	activities *liveactivity.Driver
	// What a phone should dial, the relay when there is one; set once listening.
	invite atomic.Value
	// RepoGo's own MCP server, and its address on the loopback server once listening.
	tools    *repogomcp.Server
	toolsURL atomic.Value
}

// New builds and wires every service, starting nothing: no worker runs and no
// device is admitted until Listen and Start. A failure releases what was acquired.
func New(parent context.Context, cfg Config) (*Host, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.State, 0o700); err != nil {
		return nil, err
	}
	h := &Host{cfg: cfg, log: cfg.Log}
	h.ctx, h.cancel = context.WithCancel(parent)
	for _, open := range []func() error{h.openAgents, h.openDevices, h.openChats, h.openProjects, h.openTransports} {
		if err := open(); err != nil {
			h.Close()
			return nil, err
		}
	}
	return h, nil
}

func (c Config) validate() error {
	var missing []string
	for _, f := range []struct {
		name  string
		unset bool
	}{
		{"State", c.State == ""}, {"Home", c.Home == ""}, {"Projects", c.Projects == ""},
		{"Attachments", c.Attachments == ""}, {"Token", c.Token == ""}, {"ServerID", c.ServerID == ""},
		{"Label", c.Label == ""}, {"Gateway", c.Gateway == ""}, {"Getenv", c.Getenv == nil},
		{"Update.Version", c.Update.Version == ""}, {"Update.Binary", c.Update.Binary == ""},
		{"Update.Marker", c.Update.Marker == ""}, {"Update.Latest", c.Update.Latest == nil},
		{"Update.Stage", c.Update.Stage == nil}, {"Update.Restart", c.Update.Restart == nil},
		{"Agents", c.Agents == nil}, {"InstallHooks", c.InstallHooks == nil}, {"Power", c.Power == nil},
		{"PublicIP", c.PublicIP == nil}, {"Log", c.Log == nil},
	} {
		if f.unset {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("host: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

func (h *Host) onClose(fn func()) { h.closers = append(h.closers, fn) }

// spawn runs fn as an owned worker: Close cancels its context and waits for it.
func (h *Host) spawn(fn func(context.Context)) {
	h.workers.Go(func() { fn(h.ctx) })
}

// Listen binds the loopback server; devices are admitted from here on. The
// address tells a caller that asked for Port 0 which port to publish.
func (h *Host) Listen() (*net.TCPAddr, error) {
	addr, err := h.srv.Listen()
	if err != nil {
		return nil, err
	}
	tcp := addr.(*net.TCPAddr)
	// Install links are served apart from the loopback API, which a tunnel never reaches.
	if err := h.Services.Builds.Listen(); err != nil {
		return nil, fmt.Errorf("build install server: %w", err)
	}
	h.info.SetPort(tcp.Port)
	h.invite.Store(cmp.Or(h.cfg.Relay, fmt.Sprintf("ws://127.0.0.1:%d/ws", tcp.Port)))
	h.toolsURL.Store(fmt.Sprintf("http://127.0.0.1:%d%s", tcp.Port, repogomcp.Path))
	return tcp, nil
}

// Start runs every owned worker. Call it once, after New and Listen.
func (h *Host) Start() {
	h.spawn(h.mgr.Watch)
	h.spawn(h.keeper.Run)
	// A cloud host's session ends on a clock; the phones hear it coming.
	h.spawn(func(ctx context.Context) { h.info.WatchStop(ctx, stopWarning, h.cloudStopping) })
	h.spawn(h.info.WatchPublicIP)
	h.spawn(h.watch.Run)
	h.spawn(func(ctx context.Context) { h.live.WatchNotices(ctx, h.bridge) })
	h.spawn(h.syncer.Run)
	h.spawn(h.Services.ProjectSync.Run)
	h.spawn(h.Services.Usage.Run)
	h.spawn(h.Services.Schedules.Run)
	h.spawn(h.bridge.Run)
	h.spawn(h.tunnels.Run)
	h.spawn(func(ctx context.Context) { h.envs.Boot(ctx, h.clones()...) })
	if h.link != nil {
		h.spawn(h.link.Run)
	}
	if h.alerts != nil {
		h.spawn(func(ctx context.Context) { h.alerts.Run(ctx, h.bridge) })
	}
	if h.activities != nil {
		h.spawn(func(ctx context.Context) { h.activities.Run(ctx, h.bridge) })
	}
}

// shutdownGrace bounds how long Close waits for loopback requests in flight.
const shutdownGrace = 5 * time.Second

// stopGrace bounds how long Close waits for workers once their context ends.
// One stuck in a call that cannot be cancelled must not keep an update from
// restarting the host.
const stopGrace = 20 * time.Second

// Close stops admitting devices, cancels the host's context, waits for every
// owned worker, and only then releases what they use, newest first. Safe to
// call more than once and after a failed New.
func (h *Host) Close() {
	h.closing.Do(func() {
		if h.srv != nil {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), shutdownGrace)
			if err := h.srv.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				h.log.Warn("loopback shutdown", "err", err)
			}
			cancel()
		}
		h.cancel()
		if !waitWithin(&h.workers, stopGrace) {
			// What they use stays open under them; the process is about to end.
			h.log.Error("workers did not stop; leaving them to the exit", "grace", stopGrace, "goroutines", goroutines())
			return
		}
		for i := len(h.closers) - 1; i >= 0; i-- {
			h.closers[i]()
		}
	})
}

// projectsChanged sends a projectsync pass's moved rows to every phone, each
// stamped with this host as projects.list stamps them.
func (h *Host) projectsChanged(c store.ProjectChange) {
	host := string(h.devices.Identity().ID)
	for i := range c.Changed {
		c.Changed[i].HostID = host
	}
	h.toEveryPhone(projectsync.Changed{Projects: c.Changed, Removed: c.Removed})
}

// toEveryPhone pushes ev to each paired device and counts those it reached;
// one that is offline catches up on its next list.
func (h *Host) toEveryPhone(ev emit.Event) int {
	sent := 0
	for _, peer := range h.devices.Peers() {
		if err := h.emitter.To(peer.ID, ev); err != nil {
			h.log.Debug("update not delivered", "event", ev.Method(), "device", peer.ID, "err", err)
			continue
		}
		sent++
	}
	return sent
}

// waitWithin waits for wg, and reports false if d passes first.
func waitWithin(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// goroutines is every goroutine's stack, grouped, for the log of a stop that hung.
func goroutines() string {
	var b strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
	return b.String()
}
