package host

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/repogo/host/internal/account"
	"github.com/repogo/host/internal/actions"
	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agentcatalog"
	"github.com/repogo/host/internal/agentusage"
	"github.com/repogo/host/internal/api"
	"github.com/repogo/host/internal/builds"
	"github.com/repogo/host/internal/chat"
	"github.com/repogo/host/internal/chatlive"
	"github.com/repogo/host/internal/chatsync"
	"github.com/repogo/host/internal/chatwire"
	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/cloudenv"
	"github.com/repogo/host/internal/cloudflare"
	"github.com/repogo/host/internal/cloudprojects"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/envsource"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/fly"
	"github.com/repogo/host/internal/forward"
	gitcore "github.com/repogo/host/internal/git"
	ghcore "github.com/repogo/host/internal/github"
	"github.com/repogo/host/internal/hostinfo"
	"github.com/repogo/host/internal/hostlink"
	"github.com/repogo/host/internal/hostsetup"
	"github.com/repogo/host/internal/hostupdate"
	"github.com/repogo/host/internal/liveactivity"
	"github.com/repogo/host/internal/mcp"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/power"
	"github.com/repogo/host/internal/project"
	"github.com/repogo/host/internal/projectsync"
	"github.com/repogo/host/internal/projectwatch"
	"github.com/repogo/host/internal/push"
	"github.com/repogo/host/internal/repogomcp"
	devicesrpc "github.com/repogo/host/internal/rpc/devices"
	"github.com/repogo/host/internal/rpc/registry"
	"github.com/repogo/host/internal/schedule"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/shipping"
	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/terminal"
	"github.com/repogo/host/internal/tunnel"
	"github.com/repogo/host/internal/vercel"
	"github.com/repogo/host/internal/wsserver"
)

// stopWarning is how long before a cloud session ends its phones are woken.
const stopWarning = 5 * time.Minute

func (h *Host) state(name ...string) string {
	return filepath.Join(append([]string{h.cfg.State}, name...)...)
}

// openAgents builds the providers and what reads them: MCP servers and transcripts.
func (h *Host) openAgents() error {
	if err := h.openTools(); err != nil {
		return err
	}
	var err error
	h.agents, err = h.cfg.Agents(agent.Dependencies{
		Context: h.ctx,
		Root:    h.cfg.ProviderRoot,
		Env:     []string{notify.OriginEnv + "=" + string(notify.OriginApp)},
		// h.mcps is opened from the providers this builds, so after them; a
		// turn cannot start before New returns.
		MCP:   func(ctx context.Context, cwd string) []agent.MCPServer { return h.mcps.ForTurn(ctx, cwd) },
		Tools: h.tools,
		Log:   h.log,
	})
	if err != nil {
		return err
	}
	h.onClose(h.agents.Close)
	h.mcps, err = mcp.Open(h.state("mcp.json"), func(ev mcp.Changed) { h.toEveryPhone(ev) }, h.agents.MCP...)
	if err != nil {
		return err
	}
	h.sessions = session.NewStore(h.agents.Sessions...)
	h.cfg.InstallHooks(h.log, h.agents.Hooks)

	s := &h.Services
	s.Agents = h.agents.Providers
	s.AgentUsage = agentusage.New(h.agents.Kinds(), h.agents.Usage...)
	s.AgentCatalog = agentcatalog.New(h.agents.Kinds(), h.agents.Catalogs...)
	// Its own database: the chat cache is disposable, and this is history the
	// transcripts may no longer hold.
	s.Usage, err = shipping.Open(h.ctx, h.state("usage.db"), h.state("cache", "model-rates.json"), h.agents.Shipping...)
	if err != nil {
		return fmt.Errorf("open usage ledger: %w", err)
	}
	h.onClose(func() { s.Usage.Close() })
	s.MCP = h.mcps
	s.Log = h.log
	return nil
}

// openTools builds RepoGo's own MCP server. What it reaches (the switch, the
// phones, the address) is opened after it and read only when a turn calls it.
func (h *Host) openTools() error {
	browser := repogomcp.NewBrowser(repogomcp.BrowserConfig{
		Phone: func(d device.ID) bool { _, err := h.devices.Peer(d); return err == nil },
		Send:  func(d device.ID, req repogomcp.Request) bool { return h.emitter.To(d, req) == nil },
		Alert: func(d device.ID, req repogomcp.Request) {
			if h.alerts != nil {
				h.alerts.BrowserRequest(h.ctx, d, req.RequestID)
			}
		},
	})
	h.tools = repogomcp.New(repogomcp.Config{
		URL:     func() string { u, _ := h.toolsURL.Load().(string); return u },
		On:      func(cwd string) bool { return h.mcps.BuiltinOn(mcp.BuiltinID, cwd) },
		Browser: browser, Builds: func() *builds.Service { return h.Services.Builds }, Log: h.log,
	})
	h.Services.Browser = browser
	return nil
}

// openDevices opens the paired devices and what reaches them: pushes, power,
// tunnels, and the host's own info.
func (h *Host) openDevices() error {
	var err error
	h.devices, err = device.Open(h.state("device.json"))
	if err != nil {
		return err
	}
	// A paired device expects an answer, so the host keeps the machine awake for it.
	h.keeper, err = h.cfg.Power(h.log, func() bool { return len(h.devices.Peers()) > 0 }, h.batteryChanged)
	if err != nil {
		return err
	}
	h.onClose(h.keeper.Release)
	h.info = hostinfo.New(hostinfo.Deps{
		Power: h.keeper, Relay: h.relayConnected, UpdateFailed: h.cfg.UpdateFailed, PublicIP: h.cfg.PublicIP,
	})
	if err := h.info.LoadCloud(h.state("cloud.json"), h.cfg.Getenv); err != nil {
		return fmt.Errorf("cloud session: %w", err)
	}
	h.pairer, err = device.OpenPairer(h.devices, h.state("pair-reusable.json"))
	if err != nil {
		return fmt.Errorf("reusable invite: %w", err)
	}
	h.log.Info("device identity", "id", h.devices.Identity().ID, "group", h.devices.GroupID(),
		"peers", len(h.devices.Peers()))

	// Producers hold the emitter; the mux sends on the device's own loopback
	// socket if it has one, else the relay, the fallback when there is one.
	h.Pushes = emit.NewMux()
	h.emitter = emit.New(h.Pushes, h.log)
	h.devices.OnRevoke(func(device.ID) { h.toEveryPhone(devicesrpc.Changed{}) })

	// Public tunnels: dialled only while one is open; every phone sees the list.
	h.tunnels, err = tunnel.Open(tunnel.Config{
		Path: h.state("tunnels.json"), Identity: h.devices.Identity(), Peers: h.devices,
		Gateway: h.cfg.Gateway, Plaintext: h.cfg.GatewayPlaintext,
		Changed: func(ev tunnel.Changed) { h.toEveryPhone(ev) }, OwnPort: h.info.Port, Log: h.log,
	})
	if err != nil {
		return err
	}
	accounts, err := account.Open(h.state("account.json"), h.devices.Identity())
	if err != nil {
		return err
	}

	s := &h.Services
	s.Host = h.info
	s.Devices = h.devices
	s.Pairer = h.pairer
	s.Online = h.online
	// Resolved late: the listener has not picked a port yet.
	s.Addr = func() string { addr, _ := h.invite.Load().(string); return addr }
	s.Fetcher = forward.NewFetcher(h.log)
	s.Pipes = forward.NewPipes(h.emitter, h.log)
	s.Account = accounts
	s.Tunnels = h.tunnels
	return nil
}

// openChats opens the chat cache and wires the turn manager and hook bridge
// into it and the live streams.
func (h *Host) openChats() error {
	var err error
	h.db, err = store.Open(h.state())
	if err != nil {
		return fmt.Errorf("open chat cache: %w", err)
	}
	h.onClose(func() { h.db.Close() })
	if n, err := h.db.InterruptOrphans(time.Now()); err != nil {
		h.log.Error("settling orphaned turns failed", "err", err)
	} else if n > 0 {
		h.log.Info("settled orphaned turns", "chats", n)
	}

	h.announcer = chatlive.NewAnnouncer(h.devices, h.emitter, h.db, h.Services.AgentCatalog.ModelLabel, h.log)
	h.syncer = chatsync.New(h.sessions, h.db, h.log)
	// h.live streams the manager's turns, so it comes after; no turn runs before New returns.
	h.mgr = agent.NewManager(h.log, agent.Hooks{
		// Every turn this host runs streams through here, not its callers: a
		// chat started here has no id to stream under until the agent opens it.
		Session:   func(st agent.TurnStatus) { h.live.StreamTurn(store.ChatID(st.ChatID), st.TurnID) },
		Status:    h.syncer.ObserveTurn,
		SaveQueue: h.db.SaveQueue,
	}, h.agents.Adapters...)
	// Turns a previous run left queued come back held, after InterruptOrphans
	// has ended the turns they waited behind.
	queues, err := h.db.LoadQueues()
	if err != nil {
		return fmt.Errorf("load queued turns: %w", err)
	}
	h.mgr.Restore(queues)
	wire := chatwire.New(h.agents.Labelers)
	h.live = chatlive.New(h.ctx, chatlive.Deps{
		Sessions: h.sessions, Store: h.db, Emit: h.emitter, Turns: h.mgr, Log: h.log,
		HostID: h.devices.Identity().ID, Attention: h.announcer.Attention, Approval: h.approval,
		Wire: wire,
	})
	// Terminal turns stream through the MessageDisplay hook; Stop closes them.
	h.bridge, err = notify.NewBridge(h.state("notify"), h.recordHook, h.live.Display, h.agents.Hooks...)
	if err != nil {
		return err
	}

	// Reading a model list spawns the agent's CLI; do it now, off the start
	// path, so the first phone to ask is answered from cache.
	var kinds []agent.Kind
	for _, a := range h.mgr.Agents() {
		kinds = append(kinds, a.Kind)
	}
	go h.Services.AgentCatalog.Warm(h.ctx, kinds)

	s := &h.Services
	s.Runner = h.mgr
	s.Store, s.Live, s.Wire = h.db, h.live, wire
	return nil
}

// openProjects opens what works on the user's projects: files, git, GitHub,
// terminals, actions, environments, chats and host updates.
func (h *Host) openProjects() error {
	// The projects layout is a root source as well: a fresh clone has no chat in it yet.
	layout := ghcore.NewLayout(h.cfg.Projects)
	// Git, projects, terminals and actions all share Files' containment. The
	// folder picker browses home and makes what it picks a root (h.db).
	projectFiles := files.New(files.Config{
		Roots: files.Union(h.db, layout), Home: h.cfg.Home, Picks: h.db, Folders: layout,
		ReadOnly: []files.ReadOnlyDir{{Path: h.cfg.Attachments, MaxBytes: agent.MaxUploadBytes}},
	})
	gitService := gitcore.New(projectFiles, h.log)
	terminals := terminal.New(projectFiles, h.emitter, h.log)
	h.onClose(terminals.Shutdown)
	acts := actions.New(projectFiles, h.log)
	h.onClose(acts.Shutdown)
	// Builds run on the host's context; Close waits for them to settle.
	appBuilds, err := builds.Open(h.ctx, builds.Config{
		Dir: h.state("builds"), Paths: projectFiles, URLs: h.tunnels.URLs, CloseTunnel: h.tunnels.ClosePort, Log: h.log,
	})
	if err != nil {
		return err
	}
	h.onClose(appBuilds.Close)

	envSources, err := envsource.Open(h.state("env-sources.json"), h.cfg.Home)
	if err != nil {
		return err
	}
	envRequests := envsource.NewRequests(envSources, h.cfg.Label,
		func(req envsource.Request) int { return h.toEveryPhone(req) },
		func(req envsource.Request) {
			if h.alerts != nil {
				h.alerts.EnvRequest(h.ctx, req.RequestID, req.HostLabel, req.Handles)
			}
		}, h.log)
	h.watch = projectwatch.New(projectwatch.Deps{Git: gitService, Totals: h.db, Pushes: h.Pushes, Log: h.log})
	gh := ghcore.New(layout, gitService, h.log)
	h.Services.GitHub = gh

	h.ChatDeps = chat.Deps{
		Self: h.devices.Identity().ID, ModelLabel: h.Services.AgentCatalog.ModelLabel, Cache: h.db, Sender: h.mgr, Sync: h.syncer,
		Transcripts: h.sessions, Files: projectFiles,
	}
	chats, err := chat.New(h.ChatDeps)
	if err != nil {
		return err
	}

	update := h.cfg.Update
	update.Turns, update.Terminals, update.Actions, update.Builds = h.mgr, terminals.Open, acts.Running, appBuilds.Running

	schedules, err := schedule.New(schedule.Deps{
		Self: h.devices.Identity().ID, Store: h.db, Chats: chats, Files: projectFiles, Agents: h.agents.Kinds(),
		Changed: func(c schedule.Changed) { h.toEveryPhone(c) }, Log: h.log,
	})
	if err != nil {
		return err
	}

	s := &h.Services
	s.Chats = chats
	s.Schedules = schedules
	s.Files = projectFiles
	// Every CLI in one inventory, in the order the Environment screen shows
	// them: source control, agents, cloud. Phones hear when a sign-in ends or
	// an install outlives its call.
	s.Tools = clitool.NewInventory(h.setupChanged, h.log)
	h.onClose(s.Tools.Close)
	s.Tools.Tools = append(append([]clitool.Tool{gh.Tool()}, h.agents.Tools()...),
		vercel.Tool(), cloudflare.Tool(), fly.Tool())
	s.CloudProjects = cloudprojects.New(projectFiles, s.Tools.Ready)
	s.CloudEnv = cloudenv.New()
	s.Projects = project.New(projectFiles, h.db)
	s.ProjectSync = projectsync.New(projectFiles, h.db, s.Projects, gh.Checkouts(), h.cfg.Home, h.projectsChanged, h.log)
	// Every commit that moves a chat row reaches the devices, and may move its
	// project's activity, which projectsync re-reads a settle later. Earlier
	// writes (the queue restored at start) had no device to reach yet.
	h.db.Notify(func(c store.Change) {
		h.announcer.Announce(c)
		s.ProjectSync.Nudge()
	})
	s.Git, s.Watch = gitService, h.watch
	s.Terminals = terminals
	s.Actions = acts
	s.Builds = appBuilds
	s.Updates = hostupdate.New(update)
	s.EnvSources, s.EnvRequests = envSources, envRequests
	return nil
}

// openTransports builds the router and the ways a device reaches it: the
// loopback server and, when there is one, the relay with APNs behind it.
func (h *Host) openTransports() error {
	var err error
	h.Router, err = registry.New(h.Services)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	api.New(h.Router, h.devices.Identity().ID).Register(mux)
	h.srv, err = wsserver.New(wsserver.Config{
		Port: h.cfg.Port, Token: h.cfg.Token, ServerID: h.cfg.ServerID, Mux: mux,
		Devices: h.devices, Router: h.Router, Pushes: h.Pushes, OnDisconnect: h.disconnected, Log: h.log,
		SelfAuthed: map[string]http.Handler{repogomcp.Path: h.tools},
	})
	if err != nil || h.cfg.Relay == "" {
		return err
	}
	h.link, err = hostlink.New(hostlink.Config{
		URL: h.cfg.Relay, Devices: h.devices, Router: h.Router, Log: h.log,
		OnDisconnect: h.disconnected,
		Pairing:      func() bool { pending, _ := h.pairer.Pending(); return pending },
	})
	if err != nil {
		return err
	}
	// A phone is on the relay unless it has a socket of its own here.
	h.Pushes.SetFallback(h.link)
	h.alerts = push.New(h.devices, h.link, h.log)
	h.activities = liveactivity.New(h.devices, h.link, h.log, h.agents.Name, h.db.ProjectName)
	return nil
}

// disconnected stops streaming to a device until it subscribes again.
func (h *Host) disconnected(d device.ID) {
	h.live.Unsubscribe(d)
	h.watch.Stop(d)
	h.emitter.LeaveAll(d)
}

// online reports whether a device has a loopback socket or a channel through the relay.
func (h *Host) online(d device.ID) bool {
	return h.srv.Online(d) || h.link != nil && h.link.Online(d)
}

func (h *Host) relayConnected() bool { return h.link != nil && h.link.Connected() }

// setupChanged tells every phone the setup state: a sign-in or an install
// outlives its caller, and changes what the CLI offers, so catalogs are re-read.
func (h *Host) setupChanged() {
	h.toEveryPhone(hostsetup.Snapshot(h.ctx, hostsetup.Sources{
		Host: h.info, GitHub: h.Services.GitHub, Tools: h.Services.Tools, Runner: h.mgr, Catalog: h.Services.AgentCatalog,
	}, true))
}

// batteryChanged tells every paired phone the battery moved; a low one wakes them.
func (h *Host) batteryChanged(b power.Battery) {
	h.toEveryPhone(b)
	if h.alerts != nil {
		h.alerts.Battery(h.ctx, b)
	}
}

func (h *Host) cloudStopping(hostinfo.Cloud) {
	if h.alerts != nil {
		h.alerts.CloudStopping(h.ctx, h.cfg.Label, stopWarning)
	}
}

func (h *Host) approval(chatID, turnID string, a *agent.Approval) {
	if h.activities != nil {
		h.activities.Approval(h.ctx, chatID, turnID, a)
	}
}

// recordHook makes a hook durable before the bridge publishes it; an error
// keeps the drop for the next drain.
func (h *Host) recordHook(n notify.Notice) error {
	err := h.syncer.ObserveHook(n)
	if err != nil {
		h.log.Error("hook ingestion failed; retaining drop for retry", "err", err)
	}
	return err
}
