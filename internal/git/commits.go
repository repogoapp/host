package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Commit is one commit the user made, with its lines counted for Analytics.
type Commit struct {
	SHA string
	// The committer date: when it landed on the branch, not when it was written.
	AtMs    int64
	Added   int64
	Removed int64
}

// logTimeout bounds a large batch of commits' line counts.
const logTimeout = time.Minute

// lockfiles are generated, not written: a dependency bump would otherwise
// count thousands of lines nobody typed.
var lockfiles = map[string]bool{
	"bun.lock": true, "bun.lockb": true, "package-lock.json": true, "yarn.lock": true,
	"pnpm-lock.yaml": true, "Cargo.lock": true, "go.sum": true, "Package.resolved": true,
	"Podfile.lock": true, "Gemfile.lock": true, "poetry.lock": true, "composer.lock": true,
	"uv.lock": true, "gradle.lockfile": true,
}

// CommitStamp changes whenever the commits Reachable would list could have:
// a branch tip, a detached HEAD, or who the user is. Two quick git runs.
func CommitStamp(ctx context.Context, dir string) string {
	tips, _ := run(ctx, dir, "for-each-ref", "--format=%(objectname)", "refs/heads")
	head, _ := resolve(ctx, dir, "HEAD")
	sum := sha256.Sum256([]byte(tips + "\x00" + head + "\x00" + email(ctx, dir)))
	return hex.EncodeToString(sum[:])
}

// Reachable is the SHAs of the user's commits on any local branch or a
// detached HEAD that touch dir, committed at or after sinceMs. Found from the
// branch tips, not by date, so history a pull or merge brings in is seen
// whatever its age. Yours is the repository's `user.email`; with none set
// nothing is. Merges are skipped: their lines are in the commits they join.
func Reachable(ctx context.Context, dir string, sinceMs int64) ([]string, error) {
	who := email(ctx, dir)
	if who == "" {
		return nil, nil
	}
	refs := []string{"--branches"}
	if _, err := resolve(ctx, dir, "HEAD"); err == nil {
		refs = append(refs, "HEAD")
	}
	args := append([]string{"rev-list"}, refs...)
	// --full-history: a path limit would otherwise follow one parent of a
	// merge and skip the commits on the other.
	args = append(args, "--no-merges", "--full-history", "--regexp-ignore-case",
		"--author=<"+regexp.QuoteMeta(who)+">",
		"--since=@"+strconv.FormatInt(sinceMs/1000, 10),
		"--", ".")
	out, err := runTimeout(ctx, dir, logTimeout, args...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// ReadCommits is each commit's time and its lines under dir. Binary files
// and lockfiles count no lines.
func ReadCommits(ctx context.Context, dir string, shas []string) ([]Commit, error) {
	if len(shas) == 0 {
		return nil, nil
	}
	out, err := runRaw(ctx, dir, logTimeout, strings.Join(shas, "\n")+"\n",
		"log", "--no-walk=unsorted", "--stdin", "--no-renames",
		"--numstat", "--format=%x1e%H%x1f%ct", "--", ".")
	if err != nil {
		return nil, err
	}
	return parseCommits(out), nil
}

// email is the repository's `user.email`, or "" when none is set.
func email(ctx context.Context, dir string) string {
	out, err := run(ctx, dir, "config", "user.email")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// parseCommits reads `--numstat --format=%x1e%H%x1f%ct`: a record per commit,
// its header line then "added\tremoved\tpath" lines.
func parseCommits(out string) []Commit {
	var commits []Commit
	for _, record := range strings.Split(out, "\x1e") {
		lines := strings.Split(strings.TrimSpace(record), "\n")
		sha, at, ok := strings.Cut(lines[0], "\x1f")
		if !ok || sha == "" {
			continue
		}
		seconds, err := strconv.ParseInt(strings.TrimSpace(at), 10, 64)
		if err != nil {
			continue
		}
		c := Commit{SHA: sha, AtMs: seconds * 1000}
		for _, line := range lines[1:] {
			fields := strings.SplitN(line, "\t", 3)
			if len(fields) != 3 || fields[0] == "-" || lockfiles[path.Base(strings.Trim(fields[2], `"`))] {
				continue
			}
			added, _ := strconv.ParseInt(fields[0], 10, 64)
			removed, _ := strconv.ParseInt(fields[1], 10, 64)
			c.Added += added
			c.Removed += removed
		}
		commits = append(commits, c)
	}
	return commits
}
