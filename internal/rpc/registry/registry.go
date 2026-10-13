// Package registry wires every family into one router; families never import
// each other.
package registry

import (
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	actionscore "github.com/repogoapp/host/internal/actions"
	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentcatalog"
	"github.com/repogoapp/host/internal/agentusage"
	"github.com/repogoapp/host/internal/chat"
	"github.com/repogoapp/host/internal/chatlive"
	"github.com/repogoapp/host/internal/chatwire"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/envsource"
	"github.com/repogoapp/host/internal/files"
	forwardcore "github.com/repogoapp/host/internal/forward"
	gitcore "github.com/repogoapp/host/internal/git"
	ghcore "github.com/repogoapp/host/internal/github"
	"github.com/repogoapp/host/internal/hostinfo"
	"github.com/repogoapp/host/internal/hostsetup"
	"github.com/repogoapp/host/internal/hostupdate"
	mcpcore "github.com/repogoapp/host/internal/mcp"
	projectcore "github.com/repogoapp/host/internal/project"
	"github.com/repogoapp/host/internal/projectsync"
	"github.com/repogoapp/host/internal/repogomcp"
	"github.com/repogoapp/host/internal/rpc"
	actionsrpc "github.com/repogoapp/host/internal/rpc/actions"
	browserrpc "github.com/repogoapp/host/internal/rpc/browser"
	"github.com/repogoapp/host/internal/rpc/chats"
	"github.com/repogoapp/host/internal/rpc/devices"
	envrpc "github.com/repogoapp/host/internal/rpc/env"
	forwardrpc "github.com/repogoapp/host/internal/rpc/forward"
	fsrpc "github.com/repogoapp/host/internal/rpc/fs"
	gitrpc "github.com/repogoapp/host/internal/rpc/git"
	ghrpc "github.com/repogoapp/host/internal/rpc/github"
	"github.com/repogoapp/host/internal/rpc/host"
	mcprpc "github.com/repogoapp/host/internal/rpc/mcp"
	"github.com/repogoapp/host/internal/rpc/pairing"
	portsrpc "github.com/repogoapp/host/internal/rpc/ports"
	projectsrpc "github.com/repogoapp/host/internal/rpc/projects"
	schedulesrpc "github.com/repogoapp/host/internal/rpc/schedules"
	shippingrpc "github.com/repogoapp/host/internal/rpc/shipping"
	terminalsrpc "github.com/repogoapp/host/internal/rpc/terminals"
	toolsrpc "github.com/repogoapp/host/internal/rpc/tools"
	tunnelsrpc "github.com/repogoapp/host/internal/rpc/tunnels"
	"github.com/repogoapp/host/internal/rpc/turns"
	"github.com/repogoapp/host/internal/schedule"
	"github.com/repogoapp/host/internal/shipping"
	"github.com/repogoapp/host/internal/store"
	"github.com/repogoapp/host/internal/terminal"
	"github.com/repogoapp/host/internal/tunnel"
)

// Config is what the host has to offer. Every field is required: New refuses
// a host built without one rather than serving a method that cannot work.
type Config struct {
	Host    *hostinfo.Service
	Devices *device.Store
	Pairer  *device.Pairer
	// Online reports whether a device holds a live channel, resolved late:
	// the transports are built after the router.
	Online func(device.ID) bool
	// PushChanged hears each token a phone registers or clears, by kind.
	PushChanged func(kind string)

	// Addr is the address a pairing invite advertises, resolved late.
	Addr func() string

	Runner *agent.Manager

	// Updates replaces and restarts this host.
	Updates *hostupdate.Updater

	// Tools is every CLI the host installs, updates and signs in: gh and the agents.
	Tools *clitool.Inventory
	// Agents is each agent's definition, for its resume command.
	Agents agent.Providers

	// Chats is every chat workflow; Live streams one to the devices watching.
	Chats *chat.Service
	Live  *chatlive.Manager
	// Wire builds transcript rows as the phone receives them.
	Wire *chatwire.Builder

	// Forward carries a phone's localhost connections to this machine's own dev servers.
	Fetcher *forwardcore.Fetcher
	Pipes   *forwardcore.Pipes

	// Files browses the user's projects; git, terminals and actions share its containment.
	Files *files.Service
	// Watch holds one watcher per open folder for the devices watching it.
	Watch fsrpc.Watcher

	// Store is the chat cache: the chats a device copies and the project list;
	// ProjectSync keeps the project list current.
	Store       *store.Store
	ProjectSync *projectsync.Syncer

	AgentUsage   *agentusage.Service
	AgentCatalog *agentcatalog.Service

	// Usage is the token totals the phone uploads to the leaderboard.
	Usage *shipping.Ledger

	Projects  *projectcore.Service
	Git       *gitcore.Service
	Terminals *terminal.Manager
	Actions   *actionscore.Service

	// GitHub is how a project gets onto this host in the first place.
	GitHub *ghcore.Service

	// MCP is the servers this host hands its agents; Browser brokers the
	// browser actions RepoGo's own server sends the phones.
	MCP     *mcpcore.Service
	Browser *repogomcp.Browser

	// EnvSources is the dotenv files bound here; EnvRequests brokers this
	// host's own starts.
	EnvSources  *envsource.Sources
	EnvRequests *envsource.Requests

	// Tunnels serves this host's public URLs.
	Tunnels *tunnel.Service

	// Schedules is the prompts this host starts as new chats at set times.
	Schedules *schedule.Scheduler

	Log *slog.Logger
}

func New(cfg Config) (*rpc.Router, error) {
	if missing := unset(cfg); len(missing) > 0 {
		return nil, fmt.Errorf("registry: missing %s", strings.Join(missing, ", "))
	}

	r := rpc.New(cfg.Log)
	host.Register(r, host.Deps{Service: cfg.Host, Updates: cfg.Updates,
		Setup: hostsetup.Sources{
			Host: cfg.Host, GitHub: cfg.GitHub, Tools: cfg.Tools, Runner: cfg.Runner, Catalog: cfg.AgentCatalog,
		}})
	turns.Register(r, turns.Deps{
		Runner: cfg.Runner, Usage: cfg.AgentUsage,
	})
	devices.Register(r, devices.Deps{Store: cfg.Devices, Pairer: cfg.Pairer, Online: cfg.Online, PushChanged: cfg.PushChanged})
	pairing.Register(r, pairing.Deps{Store: cfg.Devices, Pairer: cfg.Pairer, Addr: cfg.Addr})
	fsrpc.Register(r, fsrpc.Deps{Files: cfg.Files, Watch: cfg.Watch})
	projectsrpc.Register(r, projectsrpc.Deps{
		Projects: cfg.Projects, Self: cfg.Devices.Identity().ID, Store: cfg.Store, Sync: cfg.ProjectSync, Files: cfg.Files,
	})
	gitrpc.Register(r, gitrpc.Deps{Git: cfg.Git})
	ghrpc.Register(r, ghrpc.Deps{GitHub: cfg.GitHub})
	toolsrpc.Register(r, toolsrpc.Deps{Tools: cfg.Tools})
	shippingrpc.Register(r, shippingrpc.Deps{Usage: cfg.Usage, ModelLabel: cfg.AgentCatalog.ModelLabel})
	portsrpc.Register(r)
	forwardrpc.Register(r, forwardrpc.Deps{Fetcher: cfg.Fetcher, Pipes: cfg.Pipes})
	terminalsrpc.Register(r, terminalsrpc.Deps{Terminals: cfg.Terminals})
	actionsrpc.Register(r, actionsrpc.Deps{Actions: cfg.Actions})
	mcprpc.Register(r, mcprpc.Deps{MCP: cfg.MCP})
	browserrpc.Register(r, browserrpc.Deps{Browser: cfg.Browser})
	envrpc.Register(r, envrpc.Deps{Sources: cfg.EnvSources, Requests: cfg.EnvRequests})
	tunnelsrpc.Register(r, tunnelsrpc.Deps{Tunnels: cfg.Tunnels})
	schedulesrpc.Register(r, schedulesrpc.Deps{Schedules: cfg.Schedules})
	chats.Register(r, chats.Deps{Chats: cfg.Chats, Live: cfg.Live, Agents: cfg.Agents, Wire: cfg.Wire})
	return r, nil
}

// unset names the fields of cfg left at their zero value.
func unset(cfg Config) []string {
	var out []string
	v := reflect.ValueOf(cfg)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			out = append(out, v.Type().Field(i).Name)
		}
	}
	return out
}
