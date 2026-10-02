package session

import (
	"github.com/repogo/host/internal/agent"
)

// Subagent is what a subagent did, read from its own transcript: the work a
// chat shows as one row, opened.
type Subagent struct {
	// Kind is the agent's type as its provider names it: Claude's
	// subagent_type ("Explore"), Codex's role or task path.
	Kind string `json:"kind,omitempty"`
	// Name is Codex's nickname for the agent ("Laplace"); Claude has none.
	Name   string        `json:"name,omitempty"`
	Events []agent.Event `json:"events"`
}

// Subagent reads the transcript of the subagent that the call `callID` in
// session `id` started: a Claude Task or Agent call, or a Codex spawn_agent.
// ErrNotFound when the call started none this host can find.
func (s *Store) Subagent(id, callID string) (Subagent, error) {
	meta, p, err := s.Find(id)
	if err != nil {
		return Subagent{}, err
	}
	if reader, ok := p.(SubagentReader); ok {
		return reader.Subagent(meta, callID)
	}
	return Subagent{}, ErrNotFound
}

// SubagentReader is a Provider that can find the subagent a tool call started,
// given the session's Meta and the call's id.
type SubagentReader interface {
	Subagent(Meta, string) (Subagent, error)
}
