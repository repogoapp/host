package shipping

import (
	"context"
	"time"
)

// commitsSchema is the user's commits, for Analytics' commits per day and cost
// per commit. A row per checkout and commit: a project's scope is its folders,
// as requests' is, and History counts a commit once however many checkouts
// hold it. Rows stay when the checkout goes, as requests do; while it stays,
// its last year of rows matches what its branches hold.
var commitsSchema = []string{
	`CREATE TABLE IF NOT EXISTS commits (
	  cwd     TEXT NOT NULL,      -- the project folder it was read in
	  sha     TEXT NOT NULL,
	  at      INTEGER NOT NULL,   -- committer date, ms
	  added   INTEGER NOT NULL,   -- lines, lockfiles and binaries excluded
	  removed INTEGER NOT NULL,
	  PRIMARY KEY (cwd, sha)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS commits_at ON commits (at)`,
	`CREATE INDEX IF NOT EXISTS commits_cwd_at ON commits (cwd, at)`,
	// Each folder's branch tips, HEAD and email when last read, so an unmoved
	// one is not read again.
	`CREATE TABLE IF NOT EXISTS commit_reads (cwd TEXT PRIMARY KEY, stamp TEXT NOT NULL) WITHOUT ROWID`,
}

const (
	// backfill is how far back commits are kept in step with the branches.
	backfill = 365 * 24 * time.Hour
	// readBatch is how many new commits' lines are read per git run.
	readBatch = 500
)

// Commit is one of the user's commits.
type Commit struct {
	SHA     string `json:"sha"`
	AtMs    int64  `json:"at_ms"`
	Added   int64  `json:"added"`
	Removed int64  `json:"removed"`
}

// CommitSource is where the ledger reads commits from: the projects and git.
type CommitSource interface {
	// Checkouts is every project folder to read.
	Checkouts() ([]string, error)
	// Stamp changes whenever the folder's reachable commits could have.
	Stamp(ctx context.Context, dir string) string
	// Reachable is the SHAs of the user's commits on the folder's branches
	// at or after sinceMs.
	Reachable(ctx context.Context, dir string, sinceMs int64) ([]string, error)
	// Read is those commits' times and lines.
	Read(ctx context.Context, dir string, shas []string) ([]Commit, error)
}

// ReadCommits has every sweep read the user's new commits from src. Set it
// before Run.
func (l *Ledger) ReadCommits(src CommitSource) { l.commitSource = src }

// sweepCommits keeps each folder's rows in step with its branches, for the
// folders whose branches moved since the last sweep: commits newly on a
// branch are added, whatever their date, and ones no branch holds any more
// (amended, rebased, a deleted branch's) are removed. A folder that fails
// keeps its rows and its old stamp, and is tried again next sweep.
func (l *Ledger) sweepCommits(ctx context.Context, now time.Time) error {
	src := l.commitSource
	if src == nil {
		return nil
	}
	dirs, err := src.Checkouts()
	if err != nil {
		return err
	}
	stamps, err := l.commitStamps(ctx)
	if err != nil {
		return err
	}
	since := now.Add(-backfill).UnixMilli()
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		stamp := src.Stamp(ctx, dir)
		if was, ok := stamps[dir]; ok && was == stamp {
			continue
		}
		if err := l.syncCommits(ctx, src, dir, stamp, since); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// syncCommits reads one folder in full before writing anything, so a failed
// read never removes rows.
func (l *Ledger) syncCommits(ctx context.Context, src CommitSource, dir, stamp string, since int64) error {
	reachable, err := src.Reachable(ctx, dir, since)
	if err != nil {
		return err
	}
	stored, err := l.storedCommits(ctx, dir, since)
	if err != nil {
		return err
	}
	onBranch := make(map[string]bool, len(reachable))
	var fresh []string
	for _, sha := range reachable {
		onBranch[sha] = true
		if !stored[sha] {
			fresh = append(fresh, sha)
		}
	}
	var added []Commit
	for start := 0; start < len(fresh); start += readBatch {
		read, err := src.Read(ctx, dir, fresh[start:min(start+readBatch, len(fresh))])
		if err != nil {
			return err
		}
		added = append(added, read...)
	}
	var gone []string
	for sha := range stored {
		if !onBranch[sha] {
			gone = append(gone, sha)
		}
	}
	return l.recordCommits(ctx, dir, stamp, added, gone)
}

// storedCommits is the folder's recorded SHAs at or after since.
func (l *Ledger) storedCommits(ctx context.Context, dir string, since int64) (map[string]bool, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT sha FROM commits WHERE cwd = ? AND at >= ?`, dir, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out[sha] = true
	}
	return out, rows.Err()
}

func (l *Ledger) commitStamps(ctx context.Context) (map[string]string, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT cwd, stamp FROM commit_reads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var cwd, stamp string
		if err := rows.Scan(&cwd, &stamp); err != nil {
			return nil, err
		}
		out[cwd] = stamp
	}
	return out, rows.Err()
}

// recordCommits writes a folder's new commits, removes the ones no branch
// holds, and saves its stamp, together, so a sweep cut short is read again.
func (l *Ledger) recordCommits(ctx context.Context, dir, stamp string, commits []Commit, gone []string) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range commits {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO commits (cwd, sha, at, added, removed)
			VALUES (?, ?, ?, ?, ?)`, dir, c.SHA, c.AtMs, c.Added, c.Removed); err != nil {
			return err
		}
	}
	for _, sha := range gone {
		if _, err := tx.ExecContext(ctx, `DELETE FROM commits WHERE cwd = ? AND sha = ?`, dir, sha); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO commit_reads (cwd, stamp) VALUES (?, ?)
		ON CONFLICT (cwd) DO UPDATE SET stamp = excluded.stamp`, dir, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

// commits is the user's commits in a range within a scope, each once, oldest
// first.
func (l *Ledger) commits(ctx context.Context, where string, args []any) ([]Commit, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT sha, MIN(at), MAX(added), MAX(removed) FROM commits
		WHERE `+where+` GROUP BY sha ORDER BY 2, 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Commit{}
	for rows.Next() {
		var c Commit
		if err := rows.Scan(&c.SHA, &c.AtMs, &c.Added, &c.Removed); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
