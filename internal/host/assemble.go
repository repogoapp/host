package host

import (
	"context"
	"encoding/json"
	"fmt"
	pluginscore "github.com/repogoapp/host/internal/plugins"
	"net/http"
	"path/filepath"
	"runtime"
	"time"

	"github.com/repogoapp/host/internal/actions"
	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentcatalog"
	"github.com/repogoapp/host/internal/agentusage"
	"github.com/repogoapp/host/internal/api"
	"github.com/repogoapp/host/internal/chat"
	"github.com/repogoapp/host/internal/chatlive"
	"github.com/repogoapp/host/internal/chatsync"
	"github.com/repogoapp/host/internal/chatwire"
	"github.com/repogoapp/host/internal/clientprompt"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/connect"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/envsource"
	"github.com/repogoapp/host/internal/files"
	"github.com/repogoapp/host/internal/forward"
	gitcore "github.com/repogoapp/host/internal/git"
	ghcore "github.com/repogoapp/host/internal/github"
	"github.com/repogoapp/host/internal/hostinfo"
	"github.com/repogoapp/host/internal/hostlink"
	"github.com/repogoapp/host/internal/hostsetup"
	"github.com/repogoapp/host/internal/hostupdate"
	"github.com/repogoapp/host/internal/liveactivity"
	"github.com/repogoapp/host/internal/mcp"
	"github.com/repogoapp/host/internal/notify"
	"github.com/repogoapp/host/internal/power"
	"github.com/repogoapp/host/internal/project"
	"github.com/repogoapp/host/internal/projectsync"
	"github.com/repogoapp/host/internal/projectwatch"
	"github.com/repogoapp/host/internal/push"
	"github.com/repogoapp/host/internal/reminder"
	"github.com/repogoapp/host/internal/repogomcp"
	"github.com/repogoapp/host/internal/rpc"
	devicesrpc "github.com/repogoapp/host/internal/rpc/devices"
	"github.com/repogoapp/host/internal/rpc/registry"
	"github.com/repogoapp/host/internal/schedule"
	"github.com/repogoapp/host/internal/session"
	"github.com/repogoapp/host/internal/shipping"
	"github.com/repogoapp/host/internal/stepactivity"
	"github.com/repogoapp/host/internal/store"
	"github.com/repogoapp/host/internal/terminal"
	"github.com/repogoapp/host/internal/tunnel"
	"github.com/repogoapp/host/internal/uploads"
	"github.com/repogoapp/host/internal/wsserver"
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
		// h.mcps is opened below; a turn cannot start before New returns.
		MCP:   func(ctx context.Context, cwd string) []agent.MCPServer { return h.mcps.ForTurn(ctx, cwd) },
		Tools: h.tools,
		// Read when a session starts, so a phone's latest devices.hello counts.
		// With RepoGo's tools off on this environment, the agent runs as it would
		// in a terminal: nothing added to its system prompt.
		Instructions: func(req agent.TurnRequest) string {
			if !h.tools.On(req.Cwd) {
				return ""
			}
			prompt := repogomcp.ChatsPrompt(req.Parent != "")
			peer, err := h.devices.Peer(req.Device)
			if err != nil {
				return prompt
			}
			return prompt + clientprompt.For(peer.Client, h.projectPlugins(req.Cwd), h.mcps.Keys(req.Cwd))
		},
		Log: h.log,
	})
	if err != nil {
		return err
	}
	h.onClose(h.agents.Close)
	h.mcps, err = mcp.Open(h.state("mcp.json"), func(ev mcp.Changed) { h.toEveryDevice(ev) })
	if err != nil {
		return err
	}
	h.sessions = session.NewStore(h.agents.Sessions...)
	h.cfg.InstallHooks(h.log, h.agents.Hooks)

	s := &h.Services
	s.Agents = h.agents.Providers
	s.ProjectAgents = h.agents.ProjectAgents
	s.AgentUsage = agentusage.New(h.agents.Usage...)
	s.AgentCatalog = agentcatalog.New(h.log, h.agents.Catalogs...)
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
	h.Services.Plugins = pluginscore.New(h.ctx, pluginscore.Config{
		Runtime: h.pluginsRuntime(), Socket: h.state("plugins.sock"), DataDir: h.state("plugin-data"),
		Global: h.state("plugins"), Crons: h.state("crons.json"), LogsPath: h.state("plugin-logs.db"),
		Notify: func(n pluginscore.Notice) { h.alerts.Plugin(h.ctx, push.PluginNotice(n)) },
		Logged: func(ev pluginscore.Logged) { h.toEveryDevice(ev) },
		Emit:   func(d device.ID, ev emit.Event) error { return h.emitter.To(d, ev) },
		Host:   h.pluginHost,
		Log:    h.log,
	})
	h.onClose(h.Services.Plugins.Close)
	h.tools = repogomcp.New(repogomcp.Config{
		URL:     func() string { u, _ := h.toolsURL.Load().(string); return u },
		On:      func(string) bool { return h.mcps.BuiltinOn(mcp.BuiltinID) },
		Browser: browser, Plugins: h.Services.Plugins, MCP: func() *mcp.Service { return h.mcps },
		Router: func() *rpc.Router { return h.Router }, Log: h.log,
	})
	h.Services.Browser = browser
	return nil
}

// pluginHost runs a plugin's ctx.host as the host's own device; the router
// keeps a plugin caller to the methods agents and cards can call.
func (h *Host) pluginHost(ctx context.Context, plugin, method string, params json.RawMessage) (json.RawMessage, error) {
	if h.Router == nil {
		return nil, rpc.ErrUnavailable
	}
	caller := rpc.Caller{Device: h.devices.Identity().ID, Scope: rpc.ScopeLocal, Plugin: plugin}
	return h.Router.Call(ctx, caller, method, params)
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
		Trash: files.TrashDir(runtime.GOOS, h.cfg.Home) != "",
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
	h.devices.OnRevoke(func(device.ID) { h.toEveryDevice(devicesrpc.Changed{}) })

	// Public tunnels: dialled only while one is open; every phone sees the list.
	h.tunnels, err = tunnel.Open(tunnel.Config{
		Path: h.state("tunnels.json"), Identity: h.devices.Identity(), Peers: h.devices,
		Gateway: h.cfg.Gateway, Plaintext: h.cfg.GatewayPlaintext,
		Changed: func(ev tunnel.Changed) { h.toEveryDevice(ev) }, OwnPort: h.info.Port,
		Plugins: h.Services.Plugins, Log: h.log,
	})
	if err != nil {
		return err
	}

	s := &h.Services
	s.Host = h.info
	s.Devices = h.devices
	s.Pairer = h.pairer
	s.Online = h.online
	s.PushChanged = h.pushChanged
	s.Activities = stepactivity.New(h.devices, h.log)
	// Resolved late: the listener has not picked a port yet.
	s.Addr = func() string { addr, _ := h.invite.Load().(string); return addr }
	s.Fetcher = forward.NewFetcher(h.log)
	s.Pipes = forward.NewPipes(h.emitter, h.log)
	s.Tunnels = h.tunnels
	return nil
}

// sessionOpened streams a turn once its chat has an id. A chat another chat's
// agent started is marked first, so no device lists it in the drawer.
func (h *Host) sessionOpened(st agent.TurnStatus) {
	if st.Parent != "" {
		if err := h.db.SetChatParent(store.ChatID(st.ChatID), store.ChatID(st.Parent)); err != nil {
			h.log.Error("marking the chat's parent failed", "chat", st.ChatID, "parent", st.Parent, "err", err)
		}
	}
	h.live.StreamTurn(store.ChatID(st.ChatID), st.TurnID, st.Prompt)
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

	h.announcer = chatlive.NewAnnouncer(h.devices, h.Pushes.Attached, h.emitter, h.db, h.Services.AgentCatalog.ModelLabel, h.agents.ProjectAgentIcon, h.log)
	h.syncer = chatsync.New(h.sessions, h.db, h.log)
	// h.live streams the manager's turns, so it comes after; no turn runs before New returns.
	h.mgr = agent.NewManager(h.log, agent.Hooks{
		// Every turn this host runs streams through here, not its callers: a
		// chat started here has no id to stream under until the agent opens it.
		Session:   h.sessionOpened,
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
		Attention: h.announcer.Attention,
		Wire:      wire,
		Children:  h.syncer,
	})
	h.syncer.OnChildren(h.live.ChildrenChanged)
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
	// folder picker browses home and the drives plugged in, and makes what it
	// picks a root (h.db).
	projectFiles := files.New(files.Config{
		Roots: files.Union(h.db, layout), Home: h.cfg.Home, Volumes: files.VolumesDir(runtime.GOOS),
		Picks: h.db, Folders: layout,
		Projects: h.cfg.Projects, Trash: files.TrashDir(runtime.GOOS, h.cfg.Home),
		// The environment's own plugins and pages are every project's to read and watch,
		// and so is their data, so a card can open a bucket's file in the viewer.
		ReadOnly: []files.ReadOnlyDir{
			{Path: h.cfg.Attachments, MaxBytes: agent.MaxUploadBytes},
			{Path: h.state("plugins"), MaxBytes: files.MaxFileBytes},
			{Path: h.state("plugin-data"), MaxBytes: files.MaxFileBytes},
		},
	})
	gitService := gitcore.New(projectFiles, h.log)
	terminals := terminal.New(projectFiles, h.emitter, h.log)
	h.onClose(terminals.Shutdown)
	envSources, err := envsource.Open(h.state("env-sources.json"), h.cfg.Home)
	if err != nil {
		return err
	}
	acts, err := actions.New(actions.Deps{
		Paths: projectFiles, Store: h.db, Terminals: terminals, EnvSources: envSources,
		Changed: func(snap actions.Snapshot) { h.toEveryDevice(snap) },
		Logs:    h.state("action-runs"), Log: h.log,
	})
	if err != nil {
		return err
	}
	h.onClose(acts.Shutdown)
	h.watch = projectwatch.New(projectwatch.Deps{Git: gitService, Contain: projectFiles.ContainRead, Totals: h.db, Pushes: h.Pushes,
		Files: h.Services.Plugins.FilesChanged, Log: h.log})
	gh := ghcore.New(layout, gitService, h.log)
	h.Services.GitHub = gh

	h.ChatDeps = chat.Deps{
		Self: h.devices.Identity().ID, ModelLabel: h.Services.AgentCatalog.ModelLabel, AgentIcon: h.agents.ProjectAgentIcon, Cache: h.db, Sender: h.mgr, Sync: h.syncer,
		Transcripts: h.sessions, Files: projectFiles,
	}
	chats, err := chat.New(h.ChatDeps)
	if err != nil {
		return err
	}

	update := h.cfg.Update
	update.Turns, update.Terminals, update.Actions = h.mgr, terminals.Open, acts.Running

	schedules, err := schedule.New(schedule.Deps{
		Self: h.devices.Identity().ID, Store: h.db, Chats: chats, Files: projectFiles, Agents: h.agents.Kinds(),
		Changed: func(c schedule.Changed) { h.toEveryDevice(c) }, Log: h.log,
	})
	if err != nil {
		return err
	}

	reminders, err := reminder.Open(reminder.Deps{
		Store: h.db, Alert: h.remind, Log: h.log,
	})
	if err != nil {
		return err
	}

	staged, err := uploads.Open(h.state("uploads"), h.log)
	if err != nil {
		return err
	}
	h.onClose(staged.Close)

	s := &h.Services
	s.Chats = chats
	s.Schedules = schedules
	s.Reminders = reminders
	s.Uploads = staged
	s.Files = projectFiles
	// Every CLI in one inventory, in the order the Environment screen shows
	// them: source control, then agents. Phones hear when a sign-in ends or
	// an install outlives its call.
	s.Tools = clitool.NewInventory(h.setupChanged, h.log)
	h.onClose(s.Tools.Close)
	s.Tools.Tools = append([]clitool.Tool{gh.Tool()}, h.agents.Tools()...)
	s.Projects = project.New(projectFiles, h.db)
	s.ProjectSync = projectsync.New(projectFiles, h.db, s.Projects, h.cfg.Home, h.projectsChanged, h.log)
	// Every commit that moves a chat row reaches the devices, and may move its
	// project's activity, which projectsync re-reads a settle later. Earlier
	// writes (the queue restored at start) had no device to reach yet.
	h.db.Notify(func(c store.Change) {
		h.announcer.Announce(c)
		s.ProjectSync.Nudge()
		if h.activities != nil {
			h.activities.Changed(c)
		}
	})
	s.Git, s.Watch = gitService, h.watch
	s.Terminals = terminals
	s.Actions = acts
	s.Updates = hostupdate.New(update)
	s.EnvSources = envSources
	s.Connect = connect.New(connect.Deps{
		Sources: envSources, PluginDir: s.Plugins.Dir, SharedEnv: filepath.Join(h.state("plugins"), ".env"),
		Contain: projectFiles.Contain, Home: h.cfg.Home, Ignored: gitService.Ignored, MCP: h.mcps, CLIs: s.Tools,
		Changed: func(c connect.Changed) { h.toEveryDevice(c) }, Now: time.Now,
	})
	s.Tools.OnLoginEnd(s.Connect.CLIEnded)
	s.Home = h.cfg.Home
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
		Pairing:      h.pairer.Accepting,
	})
	if err != nil {
		return err
	}
	// A phone is on the relay unless it has a socket of its own here.
	h.Pushes.SetFallback(h.link)
	h.pushLink = h.link
	if h.cfg.PushRelay != "" && h.cfg.PushRelay != h.cfg.Relay {
		h.pushLink, err = hostlink.New(hostlink.Config{
			URL: h.cfg.PushRelay, Devices: h.devices, ControlOnly: true, Log: h.log,
		})
		if err != nil {
			return err
		}
	}
	h.alerts = push.New(h.devices, h.pushLink, h.log)
	h.activities = liveactivity.New(h.db, h.devices, h.pushLink, h.log)
	return nil
}

// pushChanged has the Live Activities look again when a phone's card token
// comes or goes, as when a reinstall clears it.
func (h *Host) pushChanged(kind string) {
	if h.activities != nil && (kind == device.PushToStart || kind == device.PushActivity) {
		h.activities.TokensChanged()
	}
	if kind == device.PushStepsStart || kind == device.PushSteps {
		h.Services.Activities.TokensChanged()
	}
}

// disconnected stops streaming to a device until it subscribes again.
func (h *Host) disconnected(d device.ID) {
	h.live.Unsubscribe(d)
	h.watch.Stop(d)
	h.Services.Plugins.Leave(d)
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
	h.toEveryDevice(hostsetup.Snapshot(h.ctx, hostsetup.Sources{
		Host: h.info, GitHub: h.Services.GitHub, Tools: h.Services.Tools, Catalog: h.Services.AgentCatalog,
	}, true))
}

// batteryChanged tells every paired phone the battery moved; a low one wakes them.
func (h *Host) batteryChanged(b power.Battery) {
	h.toEveryDevice(b)
	if h.alerts != nil {
		h.alerts.Battery(h.ctx, b)
	}
}

func (h *Host) cloudStopping(hostinfo.Cloud) {
	if h.alerts != nil {
		h.alerts.CloudStopping(h.ctx, h.cfg.Label, stopWarning)
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

// projectPlugins is what cwd's project can do with plugins, for its prompt.
func (h *Host) projectPlugins(cwd string) clientprompt.Plugins {
	switch {
	case h.Services.Plugins.Has(cwd):
		return clientprompt.HasPlugins
	case h.Services.Plugins.On():
		return clientprompt.CanAddPlugins
	}
	return clientprompt.NoPlugins
}

// pluginsRuntime is the SDK's source on a dev host (PluginsSDK), else the
// runtime this binary carries, unpacked; "" leaves project plugins off.
func (h *Host) pluginsRuntime() string {
	if h.cfg.PluginsSDK != "" {
		return filepath.Join(h.cfg.PluginsSDK, "src", "run.ts")
	}
	runtime, err := pluginscore.Unpack(h.state("plugins-runtime"))
	if err != nil {
		h.log.Warn("plugins: could not unpack the runtime", "err", err)
	}
	return runtime
}
