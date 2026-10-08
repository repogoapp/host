// Package projectsync materializes the project list projects.list reads. Written
// down rather than recomputed so a repository resolved once, a subprocess each,
// is not resolved again on every call. Rows that move go to every device.
package projectsync

import (
	"cmp"
	"context"
	"log/slog"
	"runtime"
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

	// ProjectsAt is the stored rows for paths, which a pass carries forward
	// for the folders it could not read.
	ProjectsAt(paths []string) ([]store.Project, error)
}

// Icons is the project service, narrowed to the icon hash each row carries.
// It is handed the lister's own paths.
type Icons interface {
	IconHash(path string) string
}

type Syncer struct {
	list  Lister
	db    Cache
	icons Icons
	log   *slog.Logger

	// checkouts is the one directory this host creates worktrees in. It is
	// what separates a worktree we made for a chat from one the user made by
	// hand, and it is why that separation needs nothing recorded.
	checkouts string

	// access is which folders a pass may read inside without waiting on a
	// privacy prompt.
	access *protected

	mu sync.Mutex // one pass at a time; see chatsync for the same reason

	// Told each pass's moved rows, so devices hear them without asking.
	changed func(store.ProjectChange)
	nudge   chan struct{}
}

func New(list Lister, db Cache, icons Icons, checkouts, home string, changed func(store.ProjectChange), log *slog.Logger) *Syncer {
	access := newProtected(runtime.GOOS, home)
	access.stuck = func(root string) {
		log.Warn("projectsync: not reading inside a folder until macOS allows it", "folder", root)
	}
	return &Syncer{
		list: list, db: db, icons: icons, checkouts: checkouts, access: access,
		changed: changed, nudge: make(chan struct{}, 1), log: log,
	}
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

// Run makes a pass at start, then one a settle after each nudge, until ctx
// ends. projects.list reads the table this keeps, so the first pass fills it.
func (s *Syncer) Run(ctx context.Context) {
	s.Once(ctx)
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
	var unread []int
	// One row per path, keeping the first: symlinked roots (/tmp, /private/tmp)
	// land on the same project, and a duplicate would be rewritten every pass.
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if seen[entry.Path] {
			continue
		}
		seen[entry.Path] = true
		if !s.access.readable(entry.Path) {
			unread = append(unread, len(rows))
			rows = append(rows, store.Project{
				Path:          entry.Path,
				ChatCount:     activity[entry.Path].Chats,
				ActivityAt:    cmp.Or(activity[entry.Path].ActivityAt, entry.ModTime),
				LastMessageAt: activity[entry.Path].LastMessageAt,
				Kind:          store.ProjectFolder,
			})
			continue
		}
		row := store.Project{
			Path:          entry.Path,
			ChatCount:     activity[entry.Path].Chats,
			ActivityAt:    activity[entry.Path].ActivityAt,
			LastMessageAt: activity[entry.Path].LastMessageAt,
			Kind:          kindOf(entry.Path, s.checkouts),
			IconHash:      s.icons.IconHash(entry.Path),
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

	s.carryForward(rows, unread)

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

// carryForward keeps what an earlier pass found inside the folders this one
// could not read: their kind, repository and icon stay as stored.
func (s *Syncer) carryForward(rows []store.Project, unread []int) {
	if len(unread) == 0 {
		return
	}
	paths := make([]string, len(unread))
	for i, at := range unread {
		paths[i] = rows[at].Path
	}
	stored, err := s.db.ProjectsAt(paths)
	if err != nil {
		s.log.Debug("projectsync: no stored rows to carry forward", "err", err)
		return
	}
	byPath := make(map[string]store.Project, len(stored))
	for _, p := range stored {
		byPath[p.Path] = p
	}
	for _, at := range unread {
		was, ok := byPath[rows[at].Path]
		if !ok {
			continue
		}
		rows[at].Kind, rows[at].IconHash = was.Kind, was.IconHash
		rows[at].RepoOwner, rows[at].RepoName = was.RepoOwner, was.RepoName
	}
}
