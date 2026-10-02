package agent

import "github.com/repogo/host/internal/clitool"

// Provider is one coding agent this host knows: its CLI as a tool, whose
// models it runs, and how to reopen a session. The registry fills in the
// tool's capabilities, Home and AccountPath for the host instance.
type Provider struct {
	Tool clitool.Tool

	// Whose models the agent runs, as the app's disclosure names it.
	Company string

	// The CLI's arguments that reopen one session in a terminal; nil when it
	// has none.
	ResumeArgs func(sessionID string) []string
}

// Capabilities an agent adds to its tool's. The values are wire strings the
// app decodes, so they never change.
const (
	CapabilityRun        = "run"
	CapabilityModels     = "models"
	CapabilityUsage      = "usage"
	CapabilityUsageReset = "usage_reset"
	CapabilityHistory    = "history"
	CapabilityHooks      = "hooks"
	CapabilityMCP        = "mcp"
	CapabilityShipping   = "shipping"
)

// InstallProvider is an agent whose CLI the host can find, install, update
// and sign in, configured by the Provider it returns.
type InstallProvider interface{ Definition() Provider }

// Closer is an agent holding resources, such as a running agent process, that
// the host releases at shutdown.
type Closer interface{ Close() }

// Kind is the agent the provider describes.
func (p Provider) Kind() Kind { return Kind(p.Tool.Kind) }

// Command is the CLI binary, which is also the process name a liveness scan
// looks for.
func (p Provider) Command() string { return p.Tool.Spec.Command }

// Providers is every agent's definition, for resolving an agent name as it
// arrives off the wire or a hook.
type Providers []Provider

func (ps Providers) Lookup(kind Kind) (Provider, bool) {
	return Find(ps, kind, Provider.Kind)
}

// Find is the first item whose kind is kind.
func Find[T any](items []T, kind Kind, kindOf func(T) Kind) (T, bool) {
	for _, item := range items {
		if kindOf(item) == kind {
			return item, true
		}
	}
	var zero T
	return zero, false
}
