package projectsync

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/chatsync"
	"github.com/repogo/host/internal/files"
	gitcore "github.com/repogo/host/internal/git"
	ghcore "github.com/repogo/host/internal/github"
	"github.com/repogo/host/internal/project"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
)

var benchLocal = flag.Bool("bench-local", false, "benchmark a pull-to-refresh against local history using a temporary database")

// forgetful is the cache with no repository known, so every pass pays for
// every origin: the cost of re-resolving on each pull.
type forgetful struct{ *store.Store }

func (forgetful) KnownRepos() (map[string]bool, error) { return map[string]bool{}, nil }

// BenchmarkLocalRefresh times what a pull-to-refresh could run, wired the way
// repogo/serve.go wires it, over the user's own history in a temp database.
// Git commands are the host's read-only runner (GIT_OPTIONAL_LOCKS=0).
func BenchmarkLocalRefresh(b *testing.B) {
	if !*benchLocal {
		b.Skip("pass -bench-local to read local chat history")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := store.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	chats := chatsync.New(session.NewStore(claude.New(agent.Dependencies{}).Sessions(), codex.New(agent.Dependencies{}).Sessions()), db, logger)
	start := time.Now()
	chats.Once(ctx)
	b.Logf("seed chatsync: %v", time.Since(start))

	projectsDir := filepath.Join(home, "RepoGo")
	layout := ghcore.NewLayout(projectsDir)
	checkouts := layout.Checkouts()
	projectFiles := files.New(files.Config{Roots: files.Union(db, layout)})
	gitService := gitcore.New(projectFiles, logger)
	icons := project.New(projectFiles, db)

	start = time.Now()
	New(projectFiles, db, checkouts, ignore, logger).Once(ctx)
	b.Logf("seed projectsync: %v", time.Since(start))

	entries, err := projectFiles.Projects()
	if err != nil {
		b.Fatal(err)
	}
	known, err := db.KnownRepos()
	if err != nil {
		b.Fatal(err)
	}
	var paths []string
	seen := map[string]bool{}
	for _, e := range entries {
		if !seen[e.Path] {
			seen[e.Path] = true
			paths = append(paths, e.Path)
		}
	}
	var unknown int
	for _, p := range paths {
		if !known[p] {
			unknown++
		}
	}
	b.Logf("projects: %d, repo known: %d, resolved again every warm pass: %d", len(paths), len(paths)-unknown, unknown)
	top := paths[:min(len(paths), gitcore.MaxBatch)]

	b.Run("chats-refresh", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			chats.Once(ctx)
		}
	})
	b.Run("projects-warm", func(b *testing.B) {
		s := New(projectFiles, db, checkouts, ignore, logger)
		for i := 0; i < b.N; i++ {
			s.Once(ctx)
		}
		b.ReportMetric(float64(unknown), "origin-reads/op")
	})
	b.Run("projects-cold", func(b *testing.B) {
		s := New(projectFiles, forgetful{db}, checkouts, ignore, logger)
		for i := 0; i < b.N; i++ {
			s.Once(ctx)
		}
		b.ReportMetric(float64(len(paths)), "projects/op")
	})
	b.Run("origin-each", func(b *testing.B) {
		type timing struct {
			path string
			d    time.Duration
		}
		var total time.Duration
		var times []timing
		for i := 0; i < b.N; i++ {
			times = times[:0]
			for _, p := range paths {
				t := time.Now()
				repoOf(ctx, p, checkouts)
				d := time.Since(t)
				total += d
				times = append(times, timing{p, d})
			}
		}
		sort.Slice(times, func(i, j int) bool { return times[i].d > times[j].d })
		for _, t := range times[:min(len(times), 3)] {
			b.Logf("slowest origin: %v %s", t.d, t.path)
		}
		b.ReportMetric(float64(total)/float64(b.N)/float64(len(paths))/1e6, "ms/project")
	})
	b.Run("git-status", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := gitService.Status(ctx, top); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(len(top)), "paths/op")
	})

	hashes := make(map[string]string, len(top))
	var withIcon int
	for _, p := range top {
		r, err := icons.DetectIcon(ctx, p, "")
		if err != nil {
			b.Fatal(err)
		}
		if r.ContentHash != "" {
			withIcon++
		}
		hashes[p] = r.ContentHash
	}
	b.Logf("icons: %d of %d projects have one", withIcon, len(top))
	for _, mode := range []string{"icons-full", "icons-unchanged"} {
		b.Run(mode, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				for _, p := range top {
					match := ""
					if mode == "icons-unchanged" {
						match = hashes[p]
					}
					if _, err := icons.DetectIcon(ctx, p, match); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ReportMetric(float64(len(top)), "paths/op")
		})
	}
}
