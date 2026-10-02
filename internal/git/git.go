// Package git reports on the user's projects by shelling out to the `git`
// binary, so hooks, config, credential helpers and worktrees behave exactly as
// they do for the user. Every path goes through the same containment as files.
package git

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/par"
)

const (
	// Generous for a cold repo on a spinning disk, short enough that a hung git
	// does not hold a phone's request open until it gives up.
	commandTimeout = 10 * time.Second

	// One sidebar's worth; a larger request is refused rather than fanned out.
	MaxBatch = 50

	// Concurrency across a batch. Each status is several short git invocations;
	// unbounded, fifty projects is a fork bomb on a laptop that is also running
	// an agent.
	batchWorkers = 8

	// MaxBranches bounds one listing. A long-lived repo has thousands of remote
	// refs and nobody picks one by scrolling; the list is sorted by most recent
	// commit, so the cap falls on branches nobody has touched in years.
	MaxBranches = 200
)

var ErrTooManyPaths = errkind.New(errkind.Invalid, "git: too many paths in one request")

// Status is one project's git state — what a sidebar row shows.
type Status struct {
	Path string `json:"path"`

	// Repo is false for a plain folder. Those are legitimate: "open a folder and
	// talk to an agent" works before anyone runs `git init`, so a non-repo is
	// reported plainly rather than as an error.
	Repo bool   `json:"repo"`
	Root string `json:"root,omitempty"`

	Branch   string `json:"branch,omitempty"`
	Detached bool   `json:"detached,omitempty"`

	// Head is the short SHA the checkout is on. Free: it comes out of the same
	// `rev-parse` that resolves the branch, because rev-parse answers as many
	// revisions as it is given in one invocation.
	Head string `json:"head,omitempty"`

	// Against the upstream, when there is one. A branch that has never been
	// pushed has no upstream and reports zero for both.
	Ahead  int `json:"ahead"`
	Behind int `json:"behind"`

	Staged    int `json:"staged"`
	Unstaged  int `json:"unstaged"`
	Untracked int `json:"untracked"`

	// Error carries a per-path failure instead of failing the whole batch. One
	// project that was deleted since its last chat must not blank the sidebar.
	Error string `json:"error,omitempty"`
}

// Commit stages everything (`add -A`: the worktree is ours and holds one chat's
// work) and records it under the user's own identity. Reports whether there was
// anything to commit.
func (s *Service) Commit(ctx context.Context, path, message string) (string, bool, error) {
	dir, err := s.paths.Contain(path)
	if err != nil {
		return "", false, err
	}
	if _, err := run(ctx, dir, "add", "-A"); err != nil {
		return "", false, fmt.Errorf("git: add: %w", err)
	}
	// Nothing staged is not a failure: a chat whose work is already committed
	// still ships, it just has nothing to add first.
	if _, err := run(ctx, dir, "diff", "--cached", "--quiet"); err == nil {
		head, _ := resolve(ctx, dir, "HEAD")
		return head, false, nil
	}
	if _, err := run(ctx, dir, "commit", "-m", message); err != nil {
		return "", false, fmt.Errorf("git: commit: %w", err)
	}
	head, err := resolve(ctx, dir, "HEAD")
	return head, true, err
}

// FileChange is one changed file.
type FileChange struct {
	Path string `json:"path"`

	// Git's two-letter porcelain code, e.g. " M", "??", "A ". Passed through
	// rather than translated: it is the vocabulary every git user already reads,
	// and an invented one would need its own legend.
	Code string `json:"code"`

	Added   int  `json:"added"`
	Removed int  `json:"removed"`
	Binary  bool `json:"binary,omitempty"`
}

// Branch is one branch a chat could fork from. Local and remote in one type: a
// fresh clone has one local branch and every other is `origin/…`.
type Branch struct {
	// Name as a user would type it: `main`, or `origin/fix-the-drawer`.
	Name string `json:"name"`

	Remote bool `json:"remote"`

	// Current is the branch this checkout is on. Not a candidate to fork from
	// so much as the answer to "where am I".
	Current bool `json:"current"`

	// Default is the repository's own default branch, read from `origin/HEAD`.
	// It is what a base-branch picker should have selected before it opens.
	Default bool `json:"default"`

	SHA string `json:"sha"`

	// UpdatedAt is the tip commit's date, in unix milliseconds — the sort key,
	// and the only thing that makes a list of two hundred branches readable.
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

type Service struct {
	paths    files.Container
	log      *slog.Logger
	mutation sync.Mutex
}

func New(paths files.Container, log *slog.Logger) *Service {
	return &Service{paths: paths, log: log}
}

// Contain proves a path is a known project. Exposed for the live lane, which
// must check a path before agreeing to run git in it.
func (s *Service) Contain(path string) (string, error) { return s.paths.Contain(path) }

// Status reports on several projects at once; one round trip per project would
// be dozens of relay hops to draw one sidebar.
func (s *Service) Status(ctx context.Context, paths []string) ([]Status, error) {
	if len(paths) > MaxBatch {
		return nil, fmt.Errorf("%w: %d, max %d", ErrTooManyPaths, len(paths), MaxBatch)
	}

	return par.Map(paths, batchWorkers, func(path string) Status {
		return s.status(ctx, path)
	}), nil
}

func (s *Service) status(ctx context.Context, path string) Status {
	st := Status{Path: path}

	dir, err := s.paths.Contain(path)
	if err != nil {
		st.Error = err.Error()
		return st
	}

	root, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		// Not a repo is the ordinary case for a plain folder, not a failure.
		return st
	}
	st.Repo, st.Root = true, strings.TrimSpace(root)

	// Two invocations: `--abbrev-ref` is a mode that applies to every revision
	// after it, so asking for both in one call returns the branch name twice.
	if branch, err := run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		st.Branch = strings.TrimSpace(branch)
	}
	if sha, err := run(ctx, dir, "rev-parse", "--short", "HEAD"); err == nil {
		st.Head = strings.TrimSpace(sha)
	}
	// Mid-rebase, mid-bisect, or on a tag. The short SHA is more useful than the
	// literal string "HEAD".
	if st.Branch == "HEAD" {
		st.Detached = true
		st.Branch = st.Head
	}

	// The origin is deliberately not read here: a status batch runs for fifty
	// projects every sweep, and `Service.Remote` answers it on a cached path.

	// Fails when the branch has no upstream, which is normal and not worth
	// reporting: zero ahead and zero behind is the honest answer.
	if counts, err := run(ctx, dir, "rev-list", "--left-right", "--count", "@{upstream}...HEAD"); err == nil {
		fields := strings.Fields(counts)
		if len(fields) == 2 {
			st.Behind, _ = strconv.Atoi(fields[0])
			st.Ahead, _ = strconv.Atoi(fields[1])
		}
	}

	porcelain, err := run(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if err != nil {
		st.Error = err.Error()
		return st
	}
	records := strings.Split(porcelain, "\x00")
	for i := 0; i < len(records); i++ {
		line := records[i]
		if len(line) < 4 {
			continue
		}
		index, worktree := line[0], line[1]
		// A rename or copy is followed by its old path, which is not a second file.
		if strings.ContainsAny(line[:2], "RC") {
			i++
		}
		switch {
		case index == '?' && worktree == '?':
			st.Untracked++
		default:
			// A file can be both staged and modified again since; git shows that
			// as two non-space columns, and it genuinely counts once on each
			// side. Counting it once would under-report what is uncommitted.
			if index != ' ' {
				st.Staged++
			}
			if worktree != ' ' {
				st.Unstaged++
			}
		}
	}
	return st
}

// Changes lists the files that differ from HEAD, working tree and index
// together, with line counts.
func (s *Service) Changes(ctx context.Context, path string) ([]FileChange, error) {
	dir, err := s.paths.Contain(path)
	if err != nil {
		return nil, err
	}

	// -z, or a path with a space, quote or non-ASCII byte comes back quoted and
	// misses its numstat line.
	porcelain, err := run(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if err != nil {
		return nil, err
	}
	codes := map[string]string{}
	order := []string{}
	records := strings.Split(porcelain, "\x00")
	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) < 4 {
			continue
		}
		code, name := record[:2], record[3:]
		// A rename or copy is followed by its old path; the new one exists.
		if strings.ContainsAny(code, "RC") {
			i++
		}
		if _, seen := codes[name]; !seen {
			order = append(order, name)
		}
		codes[name] = code
	}

	changes := make(map[string]*FileChange, len(order))
	for _, name := range order {
		changes[name] = &FileChange{Path: name, Code: codes[name]}
	}
	// HEAD covers staged and unstaged in one pass; without it a staged file
	// reports zero changed lines, which reads as "nothing happened".
	if stats, err := run(ctx, dir, "diff", "--numstat", "-z", "HEAD"); err == nil {
		applyNumstat(stats, changes)
	}

	out := make([]FileChange, 0, len(order))
	for _, name := range order {
		out = append(out, *changes[name])
	}
	return out, nil
}

// Branches lists what a chat could fork from, most recently committed first.
// One `for-each-ref` over every ref, so local and remote arrive in one order.
func (s *Service) Branches(ctx context.Context, path string) ([]Branch, error) {
	dir, err := s.paths.Contain(path)
	if err != nil {
		return nil, err
	}

	// Tab-separated because a branch name may contain almost anything else. The
	// full refname rather than the short one: `origin/main` and `main` are only
	// tellable apart by which tree they came out of.
	const format = "%(refname)\t%(objectname:short)\t%(committerdate:unix)\t%(HEAD)\t%(symref:short)"
	out, err := run(ctx, dir, "for-each-ref",
		"--sort=-committerdate", "--format="+format, "refs/heads", "refs/remotes")
	if err != nil {
		return nil, err
	}

	var branches []Branch
	locals := map[string]bool{}
	defaultRef := ""

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 5 {
			continue
		}
		// `origin/HEAD` is a symbolic ref rather than a branch — it is how the
		// repository states its own default, which is the one thing a picker
		// has to know and cannot guess.
		if target := fields[4]; target != "" {
			defaultRef = target
			continue
		}

		full := fields[0]
		branch := Branch{SHA: fields[1], Current: fields[3] == "*"}
		switch {
		case strings.HasPrefix(full, "refs/heads/"):
			branch.Name = strings.TrimPrefix(full, "refs/heads/")
			locals[branch.Name] = true
		case strings.HasPrefix(full, "refs/remotes/"):
			branch.Name, branch.Remote = strings.TrimPrefix(full, "refs/remotes/"), true
		default:
			continue // a tag or a note that wandered in
		}
		if seconds, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
			branch.UpdatedAt = seconds * 1000
		}
		branches = append(branches, branch)
	}

	// `origin/main` next to `main` is one branch twice, and the local one is
	// what forking from it would actually use. Dropped after the pass rather
	// than during it, because the remote may sort ahead of its local.
	out2 := branches[:0]
	for _, branch := range branches {
		if branch.Remote && locals[withoutRemote(branch.Name)] {
			continue
		}
		out2 = append(out2, branch)
	}
	branches = out2

	// The default is marked wherever it survived, under either name.
	if defaultRef != "" {
		local := withoutRemote(defaultRef)
		for i := range branches {
			if branches[i].Name == defaultRef || branches[i].Name == local {
				branches[i].Default = true
			}
		}
	}

	// Recency is the right order for the fifty branches nobody named, and the
	// wrong one for the two that matter: the branch you would fork from by
	// default, and the one you are standing on. Hoisted, order otherwise kept.
	sort.SliceStable(branches, func(i, j int) bool { return rank(branches[i]) < rank(branches[j]) })

	if len(branches) > MaxBranches {
		branches = branches[:MaxBranches]
	}
	return branches, nil
}

// DefaultBranch is whatever `origin/HEAD` points at, under its local name.
// Empty when there is no origin or the clone never recorded a HEAD, which the
// caller must tell apart from an answer.
func (s *Service) DefaultBranch(ctx context.Context, path string) string {
	dir, err := s.paths.Contain(path)
	if err != nil {
		return ""
	}
	out, err := run(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return ""
	}
	return withoutRemote(strings.TrimSpace(out))
}

func rank(b Branch) int {
	switch {
	case b.Default:
		return 0
	case b.Current:
		return 1
	default:
		return 2
	}
}

// BranchFolder is a branch name usable as one folder: `feat/x` would nest.
// Empty when nothing usable is left.
func BranchFolder(branch string) string {
	name := strings.NewReplacer("/", "-", " ", "-", ":", "-").Replace(branch)
	return strings.Trim(name, "-.")
}

// withoutRemote drops the remote from `origin/release/1.2`, leaving the branch
// name — which may itself contain slashes, so only the first segment goes.
func withoutRemote(name string) string {
	_, branch, _ := strings.Cut(name, "/")
	return branch
}

// applyNumstat folds `git diff --numstat -z` output into the change set, by
// the same parse the patch file list uses, so a rename lands on its new path.
func applyNumstat(stats string, changes map[string]*FileChange) {
	for _, stat := range numstatFiles(stats) {
		change, ok := changes[stat.Path]
		if !ok {
			continue
		}
		change.Added, change.Removed, change.Binary = stat.Added, stat.Removed, stat.Binary
	}
}
