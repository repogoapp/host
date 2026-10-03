// Command bench measures the full session→database path: discover every
// session on disk, normalize it, and write it to the SQLite cache.
//
// The shape being tested is the one the host will actually run at startup, so
// the numbers are meant to answer a product question — how long does a user
// wait before their history is queryable — not to win a microbenchmark.
//
// Parsing is CPU-bound and parallel; SQLite has exactly one writer, so writes
// funnel through a single goroutine. That asymmetry is the whole design.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
)

func main() {
	var (
		dbDir   = flag.String("dir", "/tmp/repogo-bench", "directory for cache.db and state.db")
		which   = flag.String("agent", "all", "an agent kind, or all")
		workers = flag.Int("workers", runtime.NumCPU(), "parse workers")
		limit   = flag.Int("limit", 0, "cap sessions (0 = all)")
		fresh   = flag.Bool("fresh", true, "delete the database first")
		skip    = flag.Bool("skip-unchanged", false, "skip sessions whose size+mtime match the last sync")
		batch   = flag.Int("batch", 64, "sessions per transaction")
		bulk    = flag.Bool("bulk", false, "synchronous=OFF during the run")
	)
	flag.Parse()

	if *fresh {
		os.RemoveAll(*dbDir)
	}
	if err := os.MkdirAll(*dbDir, 0o700); err != nil {
		fail(err)
	}
	dbPath := filepath.Join(*dbDir, store.CacheFile)

	// The same providers and context the host builds, so a new agent is
	// benchmarked with no change here.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	registry, err := agents.New(agent.Dependencies{Context: ctx})
	if err != nil {
		fail(err)
	}
	defer registry.Close()
	var providers []session.Provider
	for _, p := range registry.Sessions {
		if *which == "all" || string(p.Kind()) == *which {
			providers = append(providers, p)
		}
	}
	if len(providers) == 0 {
		fail(fmt.Errorf("no agent with history named %q", *which))
	}
	sessions := session.NewStore(providers...)

	db, err := store.Open(*dbDir)
	if err != nil {
		fail(err)
	}
	defer db.Close()

	// --- discover -----------------------------------------------------------
	t0 := time.Now()
	metas, errs := sessions.List()
	discover := time.Since(t0)
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "warn:", e)
	}
	if *limit > 0 && len(metas) > *limit {
		metas = metas[:*limit]
	}

	var totalBytes int64
	for _, m := range metas {
		totalBytes += m.SizeBytes
	}

	fingerprints := map[string]store.Fingerprint{}
	if *skip {
		if fingerprints, err = db.Fingerprints(); err != nil {
			fail(err)
		}
	}

	fmt.Printf("discover   %6d sessions  %8s  in %v\n", len(metas), mib(totalBytes), discover.Round(time.Millisecond))
	fmt.Printf("workers    %6d parse, 1 writer\n\n", *workers)

	// --- parse (parallel) → write (serial) ----------------------------------
	type parsed struct {
		meta   session.Meta
		events []agent.Event
	}

	in := make(chan session.Meta, *workers*2)
	out := make(chan parsed, *workers*2)

	var wg sync.WaitGroup
	var parseNanos, parseErrs, skipped int64
	var mu sync.Mutex

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range in {
				start := time.Now()
				events, err := sessions.Events(m)
				elapsed := time.Since(start).Nanoseconds()

				mu.Lock()
				parseNanos += elapsed
				if err != nil {
					parseErrs++
				}
				mu.Unlock()

				if err != nil {
					fmt.Fprintf(os.Stderr, "warn: %s %s: %v\n", m.Agent, m.ID, err)
					continue
				}
				out <- parsed{m, events}
			}
		}()
	}
	go func() { wg.Wait(); close(out) }()

	t1 := time.Now()
	go func() {
		defer close(in)
		for _, m := range metas {
			if *skip {
				if fp, ok := fingerprints[store.Key(m)]; ok && store.Unchanged(fp, m) {
					mu.Lock()
					skipped++
					mu.Unlock()
					continue
				}
			}
			in <- m
		}
	}()

	if *bulk {
		if err := db.Bulk(true); err != nil {
			fail(err)
		}
		defer db.Bulk(false)
	}

	var written, events int64
	var writeNanos int64
	var writeErrs int64
	pending := make([]store.Entry, 0, *batch)

	flush := func() {
		if len(pending) == 0 {
			return
		}
		start := time.Now()
		if err := db.SyncBatch(pending); err != nil {
			writeErrs += int64(len(pending))
		} else {
			for _, e := range pending {
				written++
				events += int64(len(e.Events))
			}
		}
		writeNanos += time.Since(start).Nanoseconds()
		pending = pending[:0]
	}

	for p := range out {
		pending = append(pending, store.Entry{Meta: p.meta, Events: p.events})
		if len(pending) >= *batch {
			flush()
		}
	}
	flush()
	wall := time.Since(t1)

	// --- report -------------------------------------------------------------
	dbSessions, dbEvents, _ := db.Counts()
	var dbBytes int64
	if fi, err := os.Stat(dbPath); err == nil {
		dbBytes = fi.Size()
	}
	if fi, err := os.Stat(dbPath + "-wal"); err == nil {
		dbBytes += fi.Size()
	}

	fmt.Printf("parsed     %6d sessions  %8d events   (cpu %v across %d workers)\n",
		written, events, time.Duration(parseNanos).Round(time.Millisecond), *workers)
	fmt.Printf("wrote      %6d sessions  %8d rows\n", dbSessions, dbEvents)
	if skipped > 0 {
		fmt.Printf("skipped    %6d unchanged\n", skipped)
	}
	if parseErrs > 0 || writeErrs > 0 {
		fmt.Printf("errors     %6d parse  %6d write\n", parseErrs, writeErrs)
	}
	fmt.Printf("db size    %8s\n\n", mib(dbBytes))

	fmt.Printf("WALL       %v\n", wall.Round(time.Millisecond))
	fmt.Printf("  serial write time %v (%.0f%% of wall)\n",
		time.Duration(writeNanos).Round(time.Millisecond),
		100*float64(writeNanos)/float64(wall.Nanoseconds()))
	secs := wall.Seconds()
	fmt.Printf("  %.0f sessions/s   %.0f events/s   %.1f MB/s of transcript\n",
		float64(written)/secs, float64(events)/secs, float64(totalBytes)/secs/(1<<20))
}

func mib(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

func fail(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}
