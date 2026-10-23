// Package chatsync keeps the host's chat cache current with the provider
// files, so a phone can page thousands of chats without reparsing gigabytes.
package chatsync

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
	"github.com/repogoapp/host/internal/store"
)

// resweep is how often the full session list is re-examined; unchanged
// sessions cost a stat each. It catches sessions no watcher reports.
const resweep = 30 * time.Second

// childrenActive is how recently a chat must have been written for a sweep to
// look at its children again: a background agent writes only its own files
// while its chat stays still, but not for days.
const childrenActive = 48 * time.Hour

// staleTurn is how long a turn run outside this host can go without a hook or
// a transcript line before it reads as interrupted.
const staleTurn = 10 * time.Minute

type Syncer struct {
	sessions *session.Store
	db       *store.Store
	log      *slog.Logger

	// One sweep at a time. Pull-to-refresh can arrive faster than a sweep
	// finishes, and two concurrent passes would parse the same files twice and
	// race each other's writes.
	mu   sync.Mutex
	wake chan struct{}

	// One chat's children are written by one pass at a time: a sweep and a
	// watching device's tick both sync them.
	childMu sync.Mutex
	// onChildren hears each chat whose children a pass changed, whichever
	// pass it was, so every device watching it is told.
	onChildren func(store.ChatID)

	// Closed when a first sweep ends: until then a cache rebuilt at startup
	// holds only some of the chats.
	imported     chan struct{}
	importedOnce sync.Once
}

func New(sessions *session.Store, db *store.Store, log *slog.Logger) *Syncer {
	return &Syncer{sessions: sessions, db: db, log: log, wake: make(chan struct{}, 1), imported: make(chan struct{})}
}

// Imported returns once a sweep has finished since the host started, running
// one if none has (after Run's, a stat per session), so a pull from a cache
// rebuilt at startup doesn't read the chats not yet imported as deleted.
func (s *Syncer) Imported(ctx context.Context) error {
	select {
	case <-s.imported:
		return nil
	default:
	}
	s.Once(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	s.markImported()
	return nil
}

func (s *Syncer) markImported() { s.importedOnce.Do(func() { close(s.imported) }) }

// Run handles hook-triggered syncs, with startup and periodic sweeps for recovery.
func (s *Syncer) Run(ctx context.Context) {
	s.Once(ctx)
	s.markImported()

	ticker := time.NewTicker(resweep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Once(ctx)
		case <-s.wake:
			s.Once(ctx)
		}
	}
}

// Once brings the cache up to date and reports how many sessions it rewrote.
// Also the pull-to-refresh path for a chat started seconds ago.
func (s *Syncer) Once(ctx context.Context) int {
	stats := s.once(ctx, 64)
	level := slog.LevelDebug
	if stats.synced > 0 {
		level = slog.LevelInfo
	}
	s.log.Log(ctx, level, "chatsync: sweep completed",
		"sessions", stats.synced, "total", stats.total, "events", stats.events,
		"errors", stats.errors, "pruned", stats.pruned, "children", stats.children, "duration", stats.duration, "list_duration", stats.list,
		"parse_duration", stats.parse, "write_duration", stats.write, "children_duration", stats.kids)
	return stats.synced
}

type syncStats struct {
	synced, total, events, errors, pruned, children int
	duration, list, parse, write, kids              time.Duration
}

func (s *Syncer) once(ctx context.Context, batchSize int) (stats syncStats) {
	s.mu.Lock()
	defer s.mu.Unlock()

	started := time.Now()
	defer func() { stats.duration = time.Since(started) }()
	metas, errs := s.sessions.List()
	stats.list = time.Since(started)
	stats.total, stats.errors = len(metas), len(errs)
	for _, err := range errs {
		// One unreadable session file must not stop the other 2,000.
		s.log.Debug("chatsync: skipped a session", "err", err)
	}

	fingerprints, err := s.db.Fingerprints()
	if err != nil {
		s.log.Warn("chatsync: could not read fingerprints, resyncing everything", "err", err)
		stats.errors++
		fingerprints = map[string]store.Fingerprint{}
	}

	pending := make([]store.Entry, 0, batchSize)

	flush := func() {
		if len(pending) == 0 {
			return
		}
		started := time.Now()
		err := s.db.SyncBatch(pending)
		stats.write += time.Since(started)
		if err != nil {
			stats.errors++
			// Fail-soft by design: a storage error must never break chat. The
			// provider files are still the truth and the next sweep retries.
			s.log.Warn("chatsync: batch failed", "err", err, "sessions", len(pending))
		} else {
			stats.synced += len(pending)
		}
		pending = pending[:0]
	}

	changed := map[string]bool{}
	for _, m := range metas {
		if ctx.Err() != nil {
			return stats
		}
		// Size and mtime together: an append-only file that changed moved at
		// least one of them, so an untouched session is never reparsed.
		if fp, ok := fingerprints[store.Key(m)]; ok && store.Unchanged(fp, m) {
			continue
		}
		changed[store.Key(m)] = true
		started := time.Now()
		events, err := s.sessions.Events(m)
		stats.parse += time.Since(started)
		if err != nil {
			stats.errors++
			s.log.Debug("chatsync: could not read session", "id", m.ID, "err", err)
			continue
		}
		stats.events += len(events)
		pending = append(pending, store.Entry{Meta: m, Events: events})
		if len(pending) >= batchSize {
			flush()
		}
	}
	flush()
	childStart := time.Now()
	stats.children = s.sweepChildren(ctx, metas, changed)
	stats.kids = time.Since(childStart)

	// Only after a complete, uninterrupted listing: a provider that failed to
	// enumerate, or a sweep cut short, would otherwise read as mass deletion.
	if len(errs) == 0 && ctx.Err() == nil {
		keep := make(map[string]bool, len(metas))
		for _, m := range metas {
			keep[store.Key(m)] = true
		}
		pruned, err := s.db.Prune(keep)
		if err != nil {
			stats.errors++
			s.log.Warn("chatsync: could not prune stale sessions", "err", err)
		}
		stats.pruned = pruned
	}
	now := time.Now()
	if settled, err := s.db.InterruptStale(now.Add(-staleTurn), now); err != nil {
		stats.errors++
		s.log.Warn("chatsync: could not settle stale turns", "err", err)
	} else if settled > 0 {
		s.log.Info("chatsync: settled stale turns", "chats", settled)
	}
	return stats
}

// OnChildren sets who hears that a chat's children changed. Set before Run.
func (s *Syncer) OnChildren(f func(store.ChatID)) { s.onChildren = f }

// sweepChildren syncs the children of every chat that changed in this sweep
// or was written lately, and reports how many chats' children moved.
func (s *Syncer) sweepChildren(ctx context.Context, metas []session.Meta, changed map[string]bool) int {
	moved := 0
	for _, m := range metas {
		if ctx.Err() != nil {
			return moved
		}
		if !changed[store.Key(m)] && time.Since(m.UpdatedAt) > childrenActive {
			continue
		}
		calls, err := s.SyncChildren(m)
		if err != nil {
			s.log.Debug("chatsync: could not sync children", "id", m.ID, "err", err)
			continue
		}
		if len(calls) > 0 {
			moved++
		}
	}
	return moved
}

// SyncChildren brings one chat's children up to date, reparsing only those
// whose files moved, and returns the calls whose agents changed. Watchers
// hear of it after the lock is let go.
func (s *Syncer) SyncChildren(meta session.Meta) ([]string, error) {
	calls, err := s.syncChildren(meta)
	if err == nil && len(calls) > 0 && s.onChildren != nil {
		s.onChildren(store.ChatID(agent.ChatID(meta.Agent, meta.ID)))
	}
	return calls, err
}

func (s *Syncer) syncChildren(meta session.Meta) ([]string, error) {
	s.childMu.Lock()
	defer s.childMu.Unlock()
	stored, err := s.db.ChildFingerprints(meta)
	if err != nil {
		return nil, err
	}
	children, err := s.sessions.Children(meta)
	if err != nil {
		return nil, err
	}
	if len(children) == 0 && len(stored) == 0 {
		return nil, nil
	}
	events := map[string][]agent.Event{}
	for _, child := range children {
		if fp, ok := stored[child.Key]; ok && fp == child.Fingerprint() {
			continue
		}
		fresh, err := s.sessions.ChildEvents(meta, child)
		if err != nil {
			s.log.Debug("chatsync: could not read a child", "id", meta.ID, "child", child.Key, "err", err)
			continue
		}
		events[child.Key] = fresh
	}
	return s.db.SyncSubagents(meta, children, events)
}
