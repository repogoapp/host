// Package projectsync materializes the project list project.list reads. Written
// down rather than recomputed so a repository resolved once, a subprocess each,
// is not resolved again on every call. Rows that move go to every device.
package projectsync

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/store"
)

// Lister is the filesystem service, narrowed to the one method this needs.
type Lister interface {
	Projects() ([]files.Entry, error)
}

// Cache is the chat cache, narrowed the same way.
type Cache interface {
	Activity() (map[string]store.Activity, error)
	SyncProjects([]store.Project) (store.ProjectChange, error)

	// KnownRepos is which projects already have a repository recorded, so a
	// pass only pays for the ones it does not know.
	KnownRepos() (map[string]bool, error)
}

type Syncer struct {
	list Lister
	db   Cache
	log  *slog.Logger

	// checkouts is the one directory this host creates worktrees in. It is
	// what separates a worktree we made for a chat from one the user made by
	// hand, and it is why that separation needs nothing recorded.
	checkouts string

	mu sync.Mutex // one pass at a time; see chatsync for the same reason

	// Told each pass's moved rows, so devices hear them without asking.
	changed func(store.ProjectChange)
	nudge   chan struct{}
}

func New(list Lister, db Cache, checkouts string, changed func(store.ProjectChange), log *slog.Logger) *Syncer {
	return &Syncer{list: list, db: db, checkouts: checkouts, changed: changed, nudge: make(chan struct{}, 1), log: log}
}

// Announce tells devices about rows a pass did not move: the user's own
// marks (a rename, a pin), so every device shows them at once.
func (s *Syncer) Announce(rows ...store.Project) {
	s.changed(store.ProjectChange{Changed: rows})
}

// settle is how long Run waits after a chat moves before re-reading, so a
// streaming turn's flushes share one pass.
const settle = time.Second

// Nudge asks Run for a pass: a chat moved, and its project's activity with it.
func (s *Syncer) Nudge() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// Run makes a pass a settle after each nudge until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.nudge:
		}
		timer := time.NewTimer(settle)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.Once(ctx)
	}
}

// Once brings the table up to date, tells devices the rows that moved, and
// reports them.
func (s *Syncer) Once(ctx context.Context) store.ProjectChange {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.list.Projects()
	if err != nil {
		s.log.Debug("projectsync: could not list projects", "err", err)
		return store.ProjectChange{}
	}
	// Decoration, like everywhere else it is read: a cache that cannot answer
	// leaves the counts at zero rather than emptying the grid.
	activity, err := s.db.Activity()
	if err != nil {
		s.log.Debug("projectsync: no chat activity", "err", err)
		activity = map[string]store.Activity{}
	}

	// Resolving a repository from an origin costs a subprocess, so it is asked
	// only for projects that have no answer yet.
	known, err := s.db.KnownRepos()
	if err != nil {
		s.log.Debug("projectsync: no known repos", "err", err)
		known = map[string]bool{}
	}

	rows := make([]store.Project, 0, len(entries))
	// One row per path, keeping the first: symlinked roots (/tmp, /private/tmp)
	// land on the same project, and a duplicate would be rewritten every pass.
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if seen[entry.Path] {
			continue
		}
		seen[entry.Path] = true
		row := store.Project{
			Path:          entry.Path,
			ChatCount:     activity[entry.Path].Chats,
			ActivityAt:    activity[entry.Path].ActivityAt,
			LastMessageAt: activity[entry.Path].LastMessageAt,
			Kind:          kindOf(entry.Path, s.checkouts),
		}
		// Left empty when it is already known: SyncProjects carries the stored
		// value forward, so a pass that did not look cannot erase what an
		// earlier one found. A plain folder has no origin of its own to read.
		if !known[entry.Path] && row.Kind != store.ProjectFolder {
			row.RepoOwner, row.RepoName = repoOf(ctx, entry.Path, s.checkouts)
		}
		// A clone nobody has run a chat in yet still has to sort somewhere, and
		// the folder's own mtime is the only evidence there is.
		if row.ActivityAt == 0 {
			row.ActivityAt = entry.ModTime
		}
		rows = append(rows, row)
	}

	change, err := s.db.SyncProjects(rows)
	if err != nil {
		s.log.Warn("projectsync: could not write projects", "err", err)
		return store.ProjectChange{}
	}
	if !change.Empty() {
		s.changed(change)
	}
	return change
}
