package git

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/repogo/host/internal/errkind"
)

var (
	ErrInvalidMutation = errkind.New(errkind.Invalid, "invalid git operation")
	ErrDetached        = errkind.New(errkind.Invalid, "git: this checkout is not on a branch")
	ErrHasOrigin       = errkind.New(errkind.Invalid, "git: this repository already has an origin remote")
)

// Under the client's two-minute call timeout, so a slow remote fails visibly.
const networkTimeout = 110 * time.Second

type CommitPushResult struct {
	Commit  string   `json:"commit"`
	Branch  string   `json:"branch"`
	Pushed  bool     `json:"pushed"`
	Shipped *Shipped `json:"shipped,omitempty"`
}

// Resolve the repository root again: git invoked from a contained subfolder can affect its ancestor.
func (s *Service) mutationRoot(ctx context.Context, path string) (string, error) {
	dir, err := s.paths.Contain(path)
	if err != nil {
		return "", err
	}
	root, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return s.paths.Contain(strings.TrimSpace(root))
}

func branchName(ctx context.Context, dir, branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.Contains(branch, "@{") {
		return fmt.Errorf("%w: a literal branch name is required", ErrInvalidMutation)
	}
	if _, err := run(ctx, dir, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("%w: invalid branch name", ErrInvalidMutation)
	}
	return nil
}

// CheckRevision refuses a client-named revision git would read as an option,
// or that cannot travel as one argument.
func CheckRevision(rev string) error {
	if strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, "\x00\r\n") {
		return fmt.Errorf("%w: invalid revision %q", ErrInvalidMutation, rev)
	}
	return nil
}

func cleanTree(ctx context.Context, dir string) error {
	status, err := run(ctx, dir, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("%w: commit or discard local changes first", ErrInvalidMutation)
	}
	return nil
}

// Pull fast-forwards a clean tree; discarding local work is ResetHard.
func (s *Service) Pull(ctx context.Context, path string) error {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	dir, err := s.mutationRoot(ctx, path)
	if err != nil {
		return err
	}
	if err = cleanTree(ctx, dir); err != nil {
		return err
	}
	_, err = RunTimeout(ctx, dir, networkTimeout, "pull", "--ff-only", "--no-rebase")
	return err
}

func (s *Service) ResetHard(ctx context.Context, path, branch string) error {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	dir, err := s.mutationRoot(ctx, path)
	if err != nil {
		return err
	}
	if err = branchName(ctx, dir, branch); err != nil {
		return err
	}
	if _, err = RunTimeout(ctx, dir, networkTimeout, "fetch", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch); err != nil {
		return err
	}
	_, err = run(ctx, dir, "reset", "--hard", "refs/remotes/origin/"+branch, "--")
	return err
}

func (s *Service) SwitchBranch(ctx context.Context, path, branch string) error {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	dir, err := s.mutationRoot(ctx, path)
	if err != nil {
		return err
	}
	if err = branchName(ctx, dir, branch); err != nil {
		return err
	}
	if err = cleanTree(ctx, dir); err != nil {
		return err
	}
	if strings.HasPrefix(branch, "origin/") {
		_, err = run(ctx, dir, "switch", "--track", "--", branch)
	} else {
		_, err = run(ctx, dir, "switch", "--", branch)
	}
	return err
}

func (s *Service) CreateBranch(ctx context.Context, path, name, fromRef string) error {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	dir, err := s.mutationRoot(ctx, path)
	if err != nil {
		return err
	}
	if err = branchName(ctx, dir, name); err != nil {
		return err
	}
	if fromRef == "" {
		fromRef = "HEAD"
	}
	if err = CheckRevision(fromRef); err != nil {
		return err
	}
	sha, err := run(ctx, dir, "rev-parse", "--verify", "--end-of-options", fromRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("%w: base reference does not name a commit", ErrInvalidMutation)
	}
	if err = cleanTree(ctx, dir); err != nil {
		return err
	}
	_, err = run(ctx, dir, "switch", "-c", name, strings.TrimSpace(sha))
	return err
}

func (s *Service) CommitPush(ctx context.Context, path, message string) (CommitPushResult, error) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	result := CommitPushResult{}
	if strings.TrimSpace(message) == "" {
		return result, fmt.Errorf("%w: commit message is required", ErrInvalidMutation)
	}
	dir, err := s.mutationRoot(ctx, path)
	if err != nil {
		return result, err
	}
	if result.Branch, err = CurrentBranch(ctx, dir); err != nil {
		return result, fmt.Errorf("%w: switch to a branch before committing", ErrInvalidMutation)
	}
	if _, err = run(ctx, dir, "remote", "get-url", "origin"); err != nil {
		return result, fmt.Errorf("%w: attach origin before committing and pushing", ErrInvalidMutation)
	}
	head, _, err := s.Commit(ctx, dir, message)
	if err != nil {
		return result, err
	}
	result.Commit = head
	if _, result.Shipped, err = s.Push(ctx, dir, result.Branch); err != nil {
		return result, fmt.Errorf("commit %s is saved locally, but push failed: %w", head, err)
	}
	result.Pushed = true
	return result, nil
}

// PrepareOrigin readies a contained folder for a new origin, as AttachRemote does:
// a repository of its own on branch, with no origin yet.
func (s *Service) PrepareOrigin(ctx context.Context, dir, branch string) error {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	return prepareOrigin(ctx, dir, branch)
}

// prepareOrigin inits a folder inside another checkout rather than giving the
// parent an origin.
func prepareOrigin(ctx context.Context, dir, branch string) error {
	if err := branchName(ctx, dir, branch); err != nil {
		return err
	}
	root, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || !samePath(strings.TrimSpace(root), dir) {
		if _, err = run(ctx, dir, "init", "-b", branch); err != nil {
			return err
		}
	}
	if _, err = run(ctx, dir, "remote", "get-url", "origin"); err == nil {
		return ErrHasOrigin
	}
	return nil
}

// samePath compares resolved paths: git names the top level after symlinks.
func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// CurrentBranch is the branch HEAD names, unborn included; ErrDetached when it names none.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	if branch := strings.TrimSpace(out); branch != "" {
		return branch, nil
	}
	return "", ErrDetached
}

// Push sends HEAD to origin as branch and tracks it, reporting whether origin
// already had it and, when not, what it shipped. `--porcelain` because
// "Everything up-to-date" goes to stderr.
func (s *Service) Push(ctx context.Context, dir, branch string) (upToDate bool, shipped *Shipped, err error) {
	// Read before pushing: afterwards the tracking ref no longer says where origin was.
	before, _ := resolve(ctx, dir, "refs/remotes/origin/"+branch)
	out, err := RunTimeout(ctx, dir, networkTimeout, "push", "--porcelain", "--set-upstream", "origin", "HEAD:refs/heads/"+branch)
	if err != nil {
		return false, nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "=\t") {
			return true, nil, nil
		}
	}
	return false, s.measureShipped(ctx, dir, branch, before), nil
}
