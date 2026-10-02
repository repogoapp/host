package notify

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/repogo/host/internal/agent"
)

// Provider is one agent CLI whose hook configuration we can wire the helper
// into. Ensure only appends and Remove only takes out the helper's handlers:
// the user's other tools live in the same file.
type Provider interface {
	// Kind is the agent name the helper is registered under, which every drop
	// carries in its `agent` field.
	Kind() agent.Kind

	// HookStatus maps one hook event to a chat status; ChatUnknown means the
	// event says nothing about the chat's state and the drop is discarded.
	HookStatus(event, notification, tool string) agent.ChatStatus

	// Displays reports whether the agent emits MessageDisplay frames, the
	// streamed text of a turn run in its own terminal.
	Displays() bool

	// Status inspects the agent's hook configuration without changing it.
	Status() AgentStatus

	Ensure(helper string) (AgentStatus, error)

	Remove(helper string) error
}

// HookSource is an agent that can report its state through hooks.
type HookSource interface {
	Hooks() Provider
}

// find is the provider registered under kind, or nil.
func find(providers []Provider, kind agent.Kind) Provider {
	for _, p := range providers {
		if p.Kind() == kind {
			return p
		}
	}
	return nil
}

// EnsureHooks runs at host startup so a fresh machine reports session state
// without the user first calling hooks.install. Failures are logged, never
// fatal: a host that cannot edit an agent's config still pairs and runs turns.
func EnsureHooks(log *slog.Logger, providers []Provider) {
	helper, err := WriteHelper()
	if err != nil {
		log.Warn("hook helper not written; agent hooks stay uninstalled", "err", err)
		return
	}
	for _, p := range providers {
		before := p.Status()
		st, err := p.Ensure(helper)
		switch {
		case err != nil:
			log.Warn("agent hooks not installed", "agent", p.Kind(), "path", before.ConfigPath, "err", err)
		case st.Conflict != "":
			log.Info("agent hooks left alone", "agent", p.Kind(), "path", st.ConfigPath, "reason", st.Conflict)
		case st.Installed && !before.Installed:
			log.Info("agent hooks installed", "agent", p.Kind(), "path", st.ConfigPath)
		default:
			log.Debug("agent hooks present", "agent", p.Kind(), "path", st.ConfigPath)
		}
	}
}

// RemoveHooks takes the helper out of every agent's config, for uninstall.
// It runs for each agent even when one fails, and returns the failures.
func RemoveHooks(providers []Provider) error {
	helper, err := HelperPath()
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range providers {
		if err := p.Remove(helper); err != nil {
			errs = append(errs, fmt.Errorf("%s hooks: %w", p.Kind(), err))
		}
	}
	return errors.Join(errs...)
}

// FileStatus reports a HookFile's wiring without modifying it.
func FileStatus(kind agent.Kind, f HookFile) AgentStatus {
	st := AgentStatus{Agent: string(kind), ConfigPath: f.Path}
	if helper, err := HelperPath(); err == nil {
		st.Installed = f.Installed(helper)
	}
	return st
}
