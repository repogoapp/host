package host

import (
	"context"

	"github.com/repogoapp/host/internal/git"
	"github.com/repogoapp/host/internal/shipping"
	"github.com/repogoapp/host/internal/store"
)

// commitSource feeds the usage ledger the user's commits: the store's
// checkouts, read with git.
type commitSource struct{ db *store.Store }

func (c commitSource) Checkouts() ([]string, error) { return c.db.GitCheckouts() }

func (commitSource) Stamp(ctx context.Context, dir string) string { return git.CommitStamp(ctx, dir) }

func (commitSource) Reachable(ctx context.Context, dir string, sinceMs int64) ([]string, error) {
	return git.Reachable(ctx, dir, sinceMs)
}

func (commitSource) Read(ctx context.Context, dir string, shas []string) ([]shipping.Commit, error) {
	read, err := git.ReadCommits(ctx, dir, shas)
	out := make([]shipping.Commit, len(read))
	for i, c := range read {
		out[i] = shipping.Commit{SHA: c.SHA, AtMs: c.AtMs, Added: c.Added, Removed: c.Removed}
	}
	return out, err
}
