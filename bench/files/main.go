// Command files measures the same session→cache path as bench/main.go with
// plain files in place of SQLite: discover every session (Claude by default),
// normalize it, and write <out>/<session id>/session.json and events.json.
//
// Files have no single writer, so each worker parses and writes its own
// session; the comparison with the SQLite bench is the point.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents"
	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/session"
)

func main() {
	defaultOut, err := apphome.Path("bench-sessions")
	if err != nil {
		fail(err)
	}
	var (
		out     = flag.String("out", defaultOut, "directory holding one folder per session")
		which   = flag.String("agent", "claude", "an agent kind, or all")
		workers = flag.Int("workers", runtime.NumCPU(), "parse+write workers")
		limit   = flag.Int("limit", 0, "cap sessions (0 = all)")
		fresh   = flag.Bool("fresh", true, "delete the output directory first")
		indent  = flag.Bool("indent", false, "pretty-print the JSON")
	)
	flag.Parse()

	if *fresh {
		if err := os.RemoveAll(*out); err != nil {
			fail(err)
		}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail(err)
	}

	// The host's own providers, so the parse is the one it runs.
	registry, err := agents.New(agent.Dependencies{})
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
	var transcriptBytes int64
	for _, m := range metas {
		transcriptBytes += m.SizeBytes
	}

	fmt.Printf("out        %s\n", *out)
	fmt.Printf("discover   %6d sessions  %8s  in %v\n", len(metas), mib(transcriptBytes), discover.Round(time.Millisecond))
	fmt.Printf("workers    %6d parse+write\n\n", *workers)

	marshal := json.Marshal
	if *indent {
		marshal = func(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
	}

	// --- parse + write (parallel) -------------------------------------------
	var (
		parseNanos, marshalNanos, writeNanos atomic.Int64
		written, events, outBytes            atomic.Int64
		parseErrs, writeErrs                 atomic.Int64
	)
	write := func(m session.Meta, evs []agent.Event) error {
		start := time.Now()
		metaJSON, err := marshal(m)
		if err != nil {
			return err
		}
		eventsJSON, err := marshal(evs)
		if err != nil {
			return err
		}
		marshalNanos.Add(time.Since(start).Nanoseconds())

		start = time.Now()
		defer func() { writeNanos.Add(time.Since(start).Nanoseconds()) }()
		dir := filepath.Join(*out, m.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "session.json"), metaJSON, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "events.json"), eventsJSON, 0o644); err != nil {
			return err
		}
		outBytes.Add(int64(len(metaJSON) + len(eventsJSON)))
		return nil
	}

	in := make(chan session.Meta, *workers*2)
	var wg sync.WaitGroup
	t1 := time.Now()
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range in {
				start := time.Now()
				evs, err := sessions.Events(m)
				parseNanos.Add(time.Since(start).Nanoseconds())
				if err != nil {
					parseErrs.Add(1)
					continue
				}
				if err := write(m, evs); err != nil {
					fmt.Fprintln(os.Stderr, "write:", m.ID, err)
					writeErrs.Add(1)
					continue
				}
				written.Add(1)
				events.Add(int64(len(evs)))
			}
		}()
	}
	for _, m := range metas {
		in <- m
	}
	close(in)
	wg.Wait()
	wall := time.Since(t1)

	// --- report -------------------------------------------------------------
	cpu := func(n int64) time.Duration { return time.Duration(n).Round(time.Millisecond) }
	fmt.Printf("parsed     %6d sessions  %8d events\n", written.Load(), events.Load())
	if parseErrs.Load() > 0 || writeErrs.Load() > 0 {
		fmt.Printf("errors     %6d parse  %6d write\n", parseErrs.Load(), writeErrs.Load())
	}
	fmt.Printf("out size   %8s\n\n", mib(outBytes.Load()))

	fmt.Printf("WALL       %v   (discover + this = %v)\n", wall.Round(time.Millisecond), (discover + wall).Round(time.Millisecond))
	fmt.Printf("  cpu across %d workers: parse %v   marshal %v   mkdir+write %v\n",
		*workers, cpu(parseNanos.Load()), cpu(marshalNanos.Load()), cpu(writeNanos.Load()))
	secs := wall.Seconds()
	fmt.Printf("  %.0f sessions/s   %.0f events/s   %.1f MB/s of transcript\n",
		float64(written.Load())/secs, float64(events.Load())/secs, float64(transcriptBytes)/secs/(1<<20))
}

func mib(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

func fail(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}
