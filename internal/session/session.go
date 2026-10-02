// Package session reads coding-agent sessions from the provider's own files,
// the source of truth, normalized into the same agent.Event stream a live turn
// produces; each provider supplies the parsing.
package session

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/errkind"
)

var ErrNotFound = errkind.New(errkind.NotFound, "session not found")

// Parse is one transcript line's events, each stamped with the line's time. A
// line without one leaves At zero, which a client reads as "unknown", not 1970.
func Parse(p Provider, line []byte) []agent.Event {
	events, timestamp := p.ParseLine(line)
	if at := agent.UnixMillis(timestamp); at != 0 {
		for i := range events {
			events[i].At = at
		}
	}
	return events
}

// Meta is what listing needs without parsing a whole transcript: everything
// here comes from the filename, a stat, or at most the first and last few lines.
type Meta struct {
	ID    string     `json:"id"`
	Agent agent.Kind `json:"agent"`
	Cwd   string     `json:"cwd"`
	Title string     `json:"title,omitempty"`

	// Path is the file to tail, the newest segment.
	Path string `json:"path"`

	// Paths is every file making up this session, oldest first. Codex writes a
	// new rollout file each time a session is resumed under the same session_id.
	Paths []string `json:"paths,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
	SizeBytes int64     `json:"size_bytes"`

	// ContextUsed is how much of the window the newest request consumed and
	// ContextSize the window it was measured against; zero means unknown. Latest
	// wins, not a sum, because a sum describes a bill rather than a window.
	ContextUsed int64 `json:"context_used,omitempty"`
	ContextSize int64 `json:"context_size,omitempty"`

	// Model is the provider's id for the model that served the newest request
	// (`claude-opus-5-5`, `gpt-6-astra`); empty when the transcript names none.
	Model string `json:"model,omitempty"`

	// Settings is what the newest turn ran with, as the transcript records it.
	Settings Settings `json:"settings"`
}

// Settings is a turn's configuration read back from a transcript, in the
// product's vocabulary rather than the provider's. Empty, or a nil FastMode,
// is unknown: the transcript did not say within the peeked window.
type Settings struct {
	PermissionMode agent.PermissionMode `json:"permission_mode,omitempty"`
	Mode           string               `json:"mode,omitempty"` // agent | plan
	ReasoningLevel string               `json:"reasoning_level,omitempty"`
	FastMode       *bool                `json:"fast_mode,omitempty"`
}

// Or is s with each unknown field taken from fallback.
func (s Settings) Or(fallback Settings) Settings {
	s.PermissionMode = cmp.Or(s.PermissionMode, fallback.PermissionMode)
	s.Mode = cmp.Or(s.Mode, fallback.Mode)
	s.ReasoningLevel = cmp.Or(s.ReasoningLevel, fallback.ReasoningLevel)
	if s.FastMode == nil {
		s.FastMode = fallback.FastMode
	}
	return s
}

// Files returns the segments in read order.
func (m Meta) Files() []string {
	if len(m.Paths) > 0 {
		return m.Paths
	}
	return []string{m.Path}
}

// Provider reads one agent's on-disk session store.
type Provider interface {
	Kind() agent.Kind

	// List enumerates sessions cheaply — stat and filename only, never a full
	// parse. A user with years of history has thousands of these.
	List() ([]Meta, error)

	// ParseLine converts one raw JSONL line into zero or more normalized events
	// and the line's RFC 3339 timestamp. It is stateless, and returning nothing
	// is normal: most lines are bookkeeping.
	ParseLine(line []byte) ([]agent.Event, string)
}

// Store fans out across providers; the map is fixed after NewStore.
type Store struct {
	providers map[agent.Kind]Provider
}

func NewStore(providers ...Provider) *Store {
	s := &Store{providers: make(map[agent.Kind]Provider, len(providers))}
	for _, p := range providers {
		s.providers[p.Kind()] = p
	}
	return s
}

// List returns every session across every provider, most recently updated
// first. Errors from one provider do not hide another's results: a user with
// Codex installed and Claude not should still see their Codex sessions.
func (s *Store) List() ([]Meta, []error) {
	var out []Meta
	var errs []error
	for _, p := range s.providers {
		metas, err := p.List()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Kind(), err))
			continue
		}
		// Filtered here, the one place every reader goes through, so a chat
		// run in a temp dir never reaches the cache, the project list, or
		// the live-session matcher.
		for _, m := range metas {
			if isExcluded(m.Cwd) {
				continue
			}
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, errs
}

func (s *Store) Find(id string) (Meta, Provider, error) {
	all, _ := s.List()
	for _, m := range all {
		if m.ID == id {
			return m, s.providers[m.Agent], nil
		}
	}
	return Meta{}, nil, ErrNotFound
}

// Events parses a session whose Meta the caller already has, skipping the O(n)
// lookup Read pays.
func (s *Store) Events(m Meta) ([]agent.Event, error) {
	p, ok := s.providers[m.Agent]
	if !ok {
		return nil, fmt.Errorf("no provider for %s", m.Agent)
	}
	return ReadAll(m, p)
}

// Read parses a whole session into normalized events.
func (s *Store) Read(id string) (Meta, []agent.Event, error) {
	meta, p, err := s.Find(id)
	if err != nil {
		return Meta{}, nil, err
	}
	events, err := ReadAll(meta, p)
	if err != nil {
		return Meta{}, nil, err
	}
	return meta, events, nil
}

// Delete removes a session's files from the provider's store. The files are
// the truth, so this is what "delete a chat" means; the cache follows.
func (s *Store) Delete(id string) error {
	meta, provider, err := s.Find(id)
	if err != nil {
		return err
	}
	if deleter, ok := provider.(Deleter); ok {
		return deleter.Delete(meta)
	}
	for _, path := range meta.Files() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Deleter removes native state that is not part of the readable transcript files.
type Deleter interface{ Delete(Meta) error }

// Renamer is a Provider that keeps a user-chosen title in its own files, so
// the name shows in the agent's CLI as well as here.
type Renamer interface {
	Rename(m Meta, title string) error
}

// Rename gives a session the user's title, in the provider's own record.
func (s *Store) Rename(id, title string) error {
	meta, p, err := s.Find(id)
	if err != nil {
		return err
	}
	r, ok := p.(Renamer)
	if !ok {
		return fmt.Errorf("%w: %s chats can't be renamed", errkind.ErrInvalid, meta.Agent)
	}
	return r.Rename(meta, title)
}

// AppendLine writes v as one JSON line at the end of a provider's file, the way
// the agent itself records a change: appended, never rewritten in place.
func AppendLine(path string, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Watch replays the session and then streams whatever the agent appends. The
// channel closes when ctx is cancelled; the sequence is monotonic across the
// replay/live boundary so a client can dedupe on it.
func (s *Store) Watch(ctx context.Context, id string) (Meta, <-chan agent.Event, error) {
	meta, p, err := s.Find(id)
	if err != nil {
		return Meta{}, nil, err
	}
	if watcher, ok := p.(SnapshotWatcher); ok {
		return meta, watcher.Watch(ctx, meta), nil
	}
	ch := make(chan agent.Event, tailBuffer)
	go tail(ctx, meta.Path, p, meta.ID, ch)
	return meta, ch, nil
}

// Source is an agent whose history this host can read.
type Source interface {
	Sessions() Provider
}

// Normalizer is a Provider that fixes up a whole transcript once it is read:
// assigning each row its turn and folding results that arrived late. Parse
// sees one line and cannot.
type Normalizer interface {
	Normalize([]agent.Event) []agent.Event
}

// FileParser is a Provider with a native parser for whole files. It is only
// tried in a build that carries one, and an error falls back to ParseLine.
type FileParser interface {
	ParseFile(path, sessionID string) ([]agent.Event, error)
}

// SnapshotReader exports non-JSONL native stores through their owning CLI.
type SnapshotReader interface {
	Snapshot(Meta) ([]agent.Event, error)
}

// SnapshotWatcher follows changes to a provider's non-JSONL store.
type SnapshotWatcher interface {
	Watch(context.Context, Meta) <-chan agent.Event
}
