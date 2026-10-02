package chatsync

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
)

var benchLocal = flag.Bool("bench-local", false, "benchmark local provider transcripts using temporary databases")

func BenchmarkLocalSync(b *testing.B) {
	if !*benchLocal {
		b.Skip("pass -bench-local to read local chat history")
	}
	providers := []struct {
		name string
		new  func() session.Provider
	}{
		{"claude", func() session.Provider { return claude.New(agent.Dependencies{}).Sessions() }},
		{"codex", func() session.Provider { return codex.New(agent.Dependencies{}).Sessions() }},
	}
	for _, provider := range providers {
		for _, mode := range []string{"initial", "refresh"} {
			for _, batch := range []int{1, 64, 256} {
				if mode == "refresh" && batch != 64 {
					continue
				}
				b.Run(fmt.Sprintf("%s/%s/batch%d", provider.name, mode, batch), func(b *testing.B) {
					b.StopTimer()
					root := b.TempDir()
					logger := slog.New(slog.NewTextHandler(io.Discard, nil))
					var list, parse, write time.Duration
					var sessions, synced, events int
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						dir := filepath.Join(root, "cache")
						if err := os.Mkdir(dir, 0700); err != nil {
							b.Fatal(err)
						}
						db, err := store.Open(dir)
						if err != nil {
							b.Fatal(err)
						}
						syncer := New(session.NewStore(provider.new()), db, logger)
						if mode == "refresh" {
							seed := syncer.once(context.Background(), batch)
							if seed.errors > 0 {
								db.Close()
								b.Fatal("initial sync reported errors")
							}
						}
						b.StartTimer()
						stats := syncer.once(context.Background(), batch)
						b.StopTimer()
						if err := db.Close(); err != nil {
							b.Fatal(err)
						}
						if err := os.RemoveAll(dir); err != nil {
							b.Fatal(err)
						}
						if stats.errors > 0 {
							b.Fatalf("sync reported %d errors", stats.errors)
						}
						if stats.total == 0 {
							b.Skip("no local sessions discovered")
						}
						if mode == "initial" && stats.synced != stats.total {
							b.Fatal("initial sync did not import every discovered session")
						}
						list += stats.list
						parse += stats.parse
						write += stats.write
						sessions += stats.total
						synced += stats.synced
						events += stats.events
					}
					n := float64(b.N)
					b.ReportMetric(float64(list)/n/1e6, "list-ms/op")
					b.ReportMetric(float64(parse)/n/1e6, "parse-ms/op")
					b.ReportMetric(float64(write)/n/1e6, "write-ms/op")
					b.ReportMetric(float64(sessions)/n, "sessions/op")
					b.ReportMetric(float64(synced)/n, "synced/op")
					b.ReportMetric(float64(events)/n, "events/op")
				})
			}
		}
	}
}
