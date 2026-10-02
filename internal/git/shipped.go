package git

import (
	"context"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Line counts are filtered and capped here, before the wire: an unfiltered
// --numstat ranks whoever last ran `npm install`.
const (
	// Counted at the cap rather than dropped, so a genuine big refactor still registers.
	maxShippedLinesPerCommit = 20_000
	maxShippedLinesPerFile   = 2_000
	maxShippedCommits        = 200
	// sha and ISO committer date per commit; \x1e keeps headers apart from numstat lines.
	shippedLogFormat = "--format=%x1e%H %cI"
)

// Shipped is what one push from the app sent to origin, for the leaderboard:
// counts only, never messages, paths or diffs.
type Shipped struct {
	EventID    string          `json:"event_id"`
	PushedAtMs int64           `json:"pushed_at_ms"`
	RepoOwner  string          `json:"repo_owner"`
	RepoName   string          `json:"repo_name"`
	Branch     string          `json:"branch"`
	Commits    []ShippedCommit `json:"commits" wire:"array"`
}

type ShippedCommit struct {
	SHA string `json:"sha"`
	// UTC committer day, so every host buckets a commit identically.
	Day          string `json:"day"`
	LinesAdded   int64  `json:"lines_added"`
	LinesDeleted int64  `json:"lines_deleted"`
	FileChanges  int64  `json:"file_changes"`
}

var excludedLockFiles = map[string]bool{
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"bun.lock": true, "bun.lockb": true, "podfile.lock": true, "cargo.lock": true,
	"go.sum": true, "gemfile.lock": true, "composer.lock": true,
	"poetry.lock": true, "package.resolved": true,
}

var excludedDirs = map[string]bool{
	"vendor": true, "node_modules": true, "generated": true, "gen": true,
	".next": true, "dist": true, "build": true, "deriveddata": true,
	"pods": true, ".yarn": true, "__generated__": true,
}

// isExcludedPath: dependency locks, vendored trees, generated output, minified bundles.
func isExcludedPath(p string) bool {
	lower := strings.ToLower(p)
	if excludedLockFiles[path.Base(lower)] {
		return true
	}
	for _, suffix := range []string{".min.js", ".min.css", ".map", ".pb.go", ".pb.swift", "_pb.ts"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	segments := strings.Split(lower, "/")
	for _, segment := range segments[:len(segments)-1] {
		if excludedDirs[segment] {
			return true
		}
	}
	return false
}

// measureShipped reads the commits a push to branch just sent: before..HEAD, or
// for a new branch (before empty) what no other origin ref already had. Nil
// when nothing countable moved or git failed; a push never fails on it.
func (s *Service) measureShipped(ctx context.Context, dir, branch, before string) *Shipped {
	args := []string{"log", "--no-merges", "--numstat", "--max-count=" + strconv.Itoa(maxShippedCommits), shippedLogFormat, "HEAD"}
	if before != "" {
		args = append(args, "^"+before)
	} else {
		args = append(args, "--not", "--exclude=refs/remotes/origin/"+branch, "--glob=refs/remotes/origin/*")
	}
	out, err := run(ctx, dir, args...)
	if err != nil {
		s.log.Warn("git: measure push", "err", err)
		return nil
	}
	commits := parseShipped(out)
	if len(commits) == 0 {
		return nil
	}
	owner, name := OwnerRepo(ctx, dir)
	return &Shipped{
		EventID:    uuid.NewString(),
		PushedAtMs: time.Now().UnixMilli(),
		RepoOwner:  owner,
		RepoName:   name,
		Branch:     branch,
		Commits:    commits,
	}
}

// parseShipped drops a commit whose every file is binary or excluded: the
// server needs file_changes > 0.
func parseShipped(output string) []ShippedCommit {
	var commits []ShippedCommit
	for _, record := range strings.Split(output, "\x1e") {
		lines := strings.Split(strings.TrimSpace(record), "\n")
		sha, date, ok := strings.Cut(lines[0], " ")
		if !ok || len(sha) != 40 {
			continue
		}
		committedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(date))
		if err != nil {
			continue
		}
		var added, deleted, files int64
		for _, line := range lines[1:] {
			columns := strings.Split(line, "\t")
			if len(columns) != 3 {
				continue
			}
			// "-" in both columns is a binary file: no lines to count.
			fileAdded, errA := strconv.ParseInt(columns[0], 10, 64)
			fileDeleted, errD := strconv.ParseInt(columns[1], 10, 64)
			if errA != nil || errD != nil || isExcludedPath(columns[2]) {
				continue
			}
			added += min(fileAdded, maxShippedLinesPerFile)
			deleted += min(fileDeleted, maxShippedLinesPerFile)
			files++
		}
		if files == 0 {
			continue
		}
		commits = append(commits, ShippedCommit{
			SHA:          sha,
			Day:          committedAt.UTC().Format("2006-01-02"),
			LinesAdded:   min(added, maxShippedLinesPerCommit),
			LinesDeleted: min(deleted, maxShippedLinesPerCommit),
			FileChanges:  files,
		})
	}
	return commits
}
