// Package chatsync keeps the host's chat cache current with the provider
// files, so a phone can page thousands of chats without reparsing gigabytes.
package chatsync

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
)

// resweep is how often the full session list is re-examined; unchanged
// sessions cost a stat each. It catches sessions no watcher reports.
const resweep = 30 * time.Second

type Syncer struct {
	sessions *session.Store
	db       *store.Store
	log      *slog.Logger

	// One sweep at a time. Pull-to-refresh can arrive faster than a sweep
	// finishes, and two concurrent passes would parse the same files twice and
	// race each other's writes.
	mu   sync.Mutex
	wake chan struct{}
}

func New(sessions *session.Store, db *store.Store, log *slog.Logger) *Syncer {
	return &Syncer{sessions: sessions, db: db, log: log, wake: make(chan struct{}, 1)}
}

// Run handles hook-triggered syncs, with startup and periodic sweeps for recovery.
func (s *Syncer) Run(ctx context.Context) {
	s.Once(ctx)

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
		"errors", stats.errors, "pruned", stats.pruned, "duration", stats.duration, "list_duration", stats.list,
		"parse_duration", stats.parse, "write_duration", stats.write)
	return stats.synced
}

type syncStats struct {
	synced, total, events, errors, pruned int
	duration, list, parse, write          time.Duration
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

	for _, m := range metas {
		if ctx.Err() != nil {
			return stats
		}
		// Size and mtime together: an append-only file that changed moved at
		// least one of them, so an untouched session is never reparsed.
		if fp, ok := fingerprints[store.Key(m)]; ok && store.Unchanged(fp, m) {
			continue
		}
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
	return stats
}
