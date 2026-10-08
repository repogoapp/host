// Package registry wires every family into one router; families never import
// each other.
package registry

import (
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/repogo/host/internal/account"
	actionscore "github.com/repogo/host/internal/actions"
	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agentcatalog"
	"github.com/repogo/host/internal/agentusage"
	buildscore "github.com/repogo/host/internal/builds"
	"github.com/repogo/host/internal/chat"
	"github.com/repogo/host/internal/chatlive"
	"github.com/repogo/host/internal/chatwire"
	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/cloudenv"
	"github.com/repogo/host/internal/cloudprojects"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/envsource"
	"github.com/repogo/host/internal/files"
	forwardcore "github.com/repogo/host/internal/forward"
	gitcore "github.com/repogo/host/internal/git"
	ghcore "github.com/repogo/host/internal/github"
	"github.com/repogo/host/internal/hostinfo"
	"github.com/repogo/host/internal/hostsetup"
	"github.com/repogo/host/internal/hostupdate"
	mcpcore "github.com/repogo/host/internal/mcp"
	projectcore "github.com/repogo/host/internal/project"
	"github.com/repogo/host/internal/projectsync"
	"github.com/repogo/host/internal/repogomcp"
	"github.com/repogo/host/internal/rpc"
	actionsrpc "github.com/repogo/host/internal/rpc/actions"
	browserrpc "github.com/repogo/host/internal/rpc/browser"
	buildsrpc "github.com/repogo/host/internal/rpc/builds"
	"github.com/repogo/host/internal/rpc/chats"
	"github.com/repogo/host/internal/rpc/cloud"
	"github.com/repogo/host/internal/rpc/devices"
	envrpc "github.com/repogo/host/internal/rpc/env"
	forwardrpc "github.com/repogo/host/internal/rpc/forward"
	fsrpc "github.com/repogo/host/internal/rpc/fs"
	gitrpc "github.com/repogo/host/internal/rpc/git"
	ghrpc "github.com/repogo/host/internal/rpc/github"
	"github.com/repogo/host/internal/rpc/host"
	mcprpc "github.com/repogo/host/internal/rpc/mcp"
	"github.com/repogo/host/internal/rpc/pairing"
	portsrpc "github.com/repogo/host/internal/rpc/ports"
	projectsrpc "github.com/repogo/host/internal/rpc/projects"
	schedulesrpc "github.com/repogo/host/internal/rpc/schedules"
	servicesrpc "github.com/repogo/host/internal/rpc/services"
	shippingrpc "github.com/repogo/host/internal/rpc/shipping"
	terminalsrpc "github.com/repogo/host/internal/rpc/terminals"
	toolsrpc "github.com/repogo/host/internal/rpc/tools"
	tunnelsrpc "github.com/repogo/host/internal/rpc/tunnels"
	"github.com/repogo/host/internal/rpc/turns"
	vercelrpc "github.com/repogo/host/internal/rpc/vercel"
	"github.com/repogo/host/internal/schedule"
	servicescore "github.com/repogo/host/internal/services"
	"github.com/repogo/host/internal/shipping"
	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/terminal"
	"github.com/repogo/host/internal/tunnel"
	"github.com/repogo/host/internal/vercel"
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

	// Addr is the address a pairing invite advertises, resolved late.
	Addr func() string

	Runner *agent.Manager

	// Updates replaces and restarts this host.
	Updates *hostupdate.Updater

	// Tools is every CLI the host installs, updates and signs in: gh, the
	// agents and the cloud CLIs.
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
	// Builds makes a project's iOS and Android apps and serves their install links.
	Builds *buildscore.Service

	// GitHub is how a project gets onto this host in the first place.
	GitHub *ghcore.Service
	// CloudProjects is the Cloud sheet's cards: the Vercel, Fly and
	// Cloudflare projects under a folder, each read from its provider.
	CloudProjects *cloudprojects.Service
	// CloudEnv is each card's environment variables, through its provider's CLI.
	CloudEnv *cloudenv.Service

	// MCP is the servers this host hands its agents; Browser brokers the
	// browser actions RepoGo's own server sends the phones.
	MCP     *mcpcore.Service
	Browser *repogomcp.Browser

	// EnvSources is the dotenv files bound here; EnvRequests brokers this
	// host's own starts.
	EnvSources  *envsource.Sources
	EnvRequests *envsource.Requests
	// Services runs each project's environment.json once a device approves the start.
	Services *servicescore.Manager

	// Account links this host to a RepoGo account; Tunnels serves its public URLs.
	Account *account.Service
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
	host.Register(r, host.Deps{Service: cfg.Host, Updates: cfg.Updates, Account: cfg.Account,
		Setup: hostsetup.Sources{
			Host: cfg.Host, GitHub: cfg.GitHub, Tools: cfg.Tools, Runner: cfg.Runner, Catalog: cfg.AgentCatalog,
		}})
	turns.Register(r, turns.Deps{
		Runner: cfg.Runner, Usage: cfg.AgentUsage,
	})
	devices.Register(r, devices.Deps{Store: cfg.Devices, Pairer: cfg.Pairer, Online: cfg.Online})
	pairing.Register(r, pairing.Deps{Store: cfg.Devices, Pairer: cfg.Pairer, Addr: cfg.Addr})
	fsrpc.Register(r, fsrpc.Deps{Files: cfg.Files, Watch: cfg.Watch})
	projectsrpc.Register(r, projectsrpc.Deps{
		Projects: cfg.Projects, Self: cfg.Devices.Identity().ID, Store: cfg.Store, Sync: cfg.ProjectSync, Files: cfg.Files,
	})
	gitrpc.Register(r, gitrpc.Deps{Git: cfg.Git})
	ghrpc.Register(r, ghrpc.Deps{GitHub: cfg.GitHub})
	cloud.Register(r, cloud.Deps{Projects: cfg.CloudProjects, Env: cfg.CloudEnv})
	vercelrpc.Register(r, vercelrpc.Deps{ReadLogs: vercel.ReadLogs, ReadDeployments: vercel.ReadDeployments})
	toolsrpc.Register(r, toolsrpc.Deps{Tools: cfg.Tools})
	shippingrpc.Register(r, shippingrpc.Deps{Usage: cfg.Usage, ModelLabel: cfg.AgentCatalog.ModelLabel})
	portsrpc.Register(r)
	forwardrpc.Register(r, forwardrpc.Deps{Fetcher: cfg.Fetcher, Pipes: cfg.Pipes})
	terminalsrpc.Register(r, terminalsrpc.Deps{Terminals: cfg.Terminals})
	actionsrpc.Register(r, actionsrpc.Deps{Actions: cfg.Actions})
	buildsrpc.Register(r, buildsrpc.Deps{Builds: cfg.Builds})
	mcprpc.Register(r, mcprpc.Deps{MCP: cfg.MCP})
	browserrpc.Register(r, browserrpc.Deps{Browser: cfg.Browser})
	envrpc.Register(r, envrpc.Deps{Sources: cfg.EnvSources, Requests: cfg.EnvRequests})
	servicesrpc.Register(r, servicesrpc.Deps{Services: cfg.Services})
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
