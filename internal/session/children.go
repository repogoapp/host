package session

import (
	"fmt"
	"time"

	"github.com/repogoapp/host/internal/agent"
)

// Child is work a session's call started, written to files of its own: a
// subagent, or one agent of a workflow run. The cache keeps it beside its
// chat and never lists it as one (store/subagents.go).
type Child struct {
	// Key names it within its parent: Claude's agent id, Codex's thread id.
	Key string
	// CallID is the call that started it, empty until the provider can tell.
	CallID string
	// Kind is its type as the provider names it ("Explore", "worker");
	// Name is Codex's nickname for it ("Laplace").
	Kind string
	Name string

	// A workflow agent's run, label and phase, its place in the order the
	// run started its agents, and the result the run recorded, if any.
	RunID  string
	Label  string
	Phase  string
	Order  int
	Done   bool
	Result string

	// Files are what its transcript is read from, oldest first.
	Files []ChildFile
}

// ChildFile is one file of a child's transcript as it stood when listed.
type ChildFile struct {
	Path    string
	Size    int64
	ModTime time.Time
	// SkipLines is how many leading lines copy another agent's history: a
	// forked Codex child starts with its parent's.
	SkipLines int
}

// Fingerprint changes whenever any of the child's files does, so an
// unchanged child is never reparsed.
func (c Child) Fingerprint() int64 {
	h := uint64(14695981039346656037)
	mix := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= 1099511628211
		}
	}
	for _, f := range c.Files {
		mix(fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00", f.Path, f.Size, f.ModTime.UnixNano(), f.SkipLines))
	}
	return int64(h)
}

// UpdatedAt is when any of its files was last written.
func (c Child) UpdatedAt() time.Time {
	var newest time.Time
	for _, f := range c.Files {
		if f.ModTime.After(newest) {
			newest = f.ModTime
		}
	}
	return newest
}

// ChildReader is a Provider whose sessions start agents that write their own
// transcripts.
type ChildReader interface {
	// Children lists what meta's calls started, each as its files stand now.
	Children(meta Meta) ([]Child, error)
	// ChildEvents reads one child's transcript.
	ChildEvents(meta Meta, child Child) ([]agent.Event, error)
}

// Children lists the agents meta's calls started; none for a provider whose
// agents write no files of their own.
func (s *Store) Children(meta Meta) ([]Child, error) {
	if reader, ok := s.providers[meta.Agent].(ChildReader); ok {
		return reader.Children(meta)
	}
	return nil, nil
}

// ChildEvents reads one child's transcript.
func (s *Store) ChildEvents(meta Meta, child Child) ([]agent.Event, error) {
	reader, ok := s.providers[meta.Agent].(ChildReader)
	if !ok {
		return nil, fmt.Errorf("%s: %w", meta.Agent, ErrNotFound)
	}
	return reader.ChildEvents(meta, child)
}
