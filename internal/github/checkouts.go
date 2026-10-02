package github

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/repogo/host/internal/files"
)

// Layout is the projects directory: loose folders at the top, clones at
// `.repos/<owner>/<repo>`, chat worktrees under `.worktrees/<owner>/<repo>/`.
// Files reads its Roots, so it exists before the Service that contains paths.
type Layout struct{ dir string }

func NewLayout(dir string) Layout { return Layout{dir: dir} }

// Checkouts is the one directory this host creates chat worktrees in.
func (l Layout) Checkouts() string { return filepath.Join(l.dir, checkoutsDir) }

// CheckoutParent is where a chat worktree of owner/repo is filed.
func CheckoutParent(checkouts, owner, repo string) string {
	return filepath.Join(checkouts, owner, repo)
}

// CheckoutRepo reads owner and repo back out of a path under checkouts.
func CheckoutRepo(checkouts, path string) (owner, repo string, ok bool) {
	if checkouts == "" || !files.Within(checkouts, path) {
		return "", "", false
	}
	rel, err := filepath.Rel(checkouts, path)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// Roots makes every clone, every chat worktree, and any plain folder directly
// under the directory a project. Read off the disk each time so it survives
// the chat cache being dropped.
func (l Layout) Roots() ([]string, error) {
	out, err := childDirs(l.dir)
	if err != nil {
		return nil, err
	}
	clones, err := l.clones()
	if err != nil {
		return nil, err
	}
	out = append(out, clones...)
	repos, err := ownerRepos(l.Checkouts())
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		checkouts, err := childDirs(repo)
		if err != nil {
			return nil, err
		}
		for _, checkout := range checkouts {
			// Everything in here was put there by us, so a folder that is not a
			// checkout is a half-finished one.
			if isCheckout(checkout) {
				out = append(out, checkout)
			}
		}
	}
	return out, nil
}

// Clones is every clone this host made, one per repository: worktrees are
// left out, since they share their clone's ports.
func (l Layout) Clones() []string {
	out, _ := l.clones()
	return out
}

// clones is every `.repos/<owner>/<repo>` that is a checkout; anything else
// there is a half-finished clone.
func (l Layout) clones() ([]string, error) {
	repos, err := ownerRepos(filepath.Join(l.dir, reposDir))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, repo := range repos {
		if isCheckout(repo) {
			out = append(out, repo)
		}
	}
	return out, nil
}

// ownerRepos is every `<base>/<owner>/<repo>` folder.
func ownerRepos(base string) ([]string, error) {
	owners, err := childDirs(base)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, owner := range owners {
		repos, err := childDirs(owner)
		if err != nil {
			return nil, err
		}
		out = append(out, repos...)
	}
	return out, nil
}

// isCheckout reports whether a folder is a git working tree: a clone's `.git`
// is a directory, a linked worktree's a file.
func isCheckout(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// childDirs lists a directory's visible subdirectories, sorted, treating a
// directory that is not there as empty: nothing exists until the first clone.
func childDirs(dir string) ([]string, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("github: read %s: %w", dir, err)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		// Skips `.repos` and `.worktrees` at the top level: they hold projects.
		if item.IsDir() && !strings.HasPrefix(item.Name(), ".") {
			out = append(out, filepath.Join(dir, item.Name()))
		}
	}
	return out, nil
}

// Clone downloads a repository into the clone directory and returns its path.
// The repository name picks a folder under one fixed directory and nothing
// else, so this can only ever make a new folder in one place.
func (s *Service) Clone(ctx context.Context, nameWithOwner string) (Clone, error) {
	nameWithOwner = strings.TrimSpace(nameWithOwner)
	if !safeName.MatchString(nameWithOwner) {
		return Clone{}, fmt.Errorf("%w: %q", ErrBadName, nameWithOwner)
	}
	owner, name, _ := strings.Cut(nameWithOwner, "/")

	// A second clone of a repository already here is a double tap.
	dest := filepath.Join(s.dir, reposDir, owner, name)
	if isCheckout(dest) {
		return Clone{NameWithOwner: nameWithOwner, Path: dest, Existed: true}, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Clone{}, fmt.Errorf("github: create clone dir: %w", err)
	}

	// Straight into place, and only into an empty folder — git's own rule — so
	// nothing already there, another clone's download included, is ever
	// removed. An empty folder a failed clone left is reused.
	if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
		return Clone{}, fmt.Errorf("%w: %s is not empty", ErrFolderExists, dest)
	}
	if _, err := s.run(ctx, cloneTimeout, "repo", "clone", nameWithOwner, dest); err != nil {
		os.Remove(dest) // only if empty: git clears what it wrote
		return Clone{}, err
	}
	go s.onClone(dest)
	return Clone{NameWithOwner: nameWithOwner, Path: dest}, nil
}

// NewFolder makes an empty project directly under the clone directory: the one
// folder a host with no projects yet can grow, since every other mkdir needs a root.
func (l Layout) NewFolder(name string) (string, error) {
	// A leading dot would make a folder Roots skips.
	if !repoName.MatchString(name) || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("%w: %q", ErrBadFolder, name)
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return "", fmt.Errorf("github: create projects dir: %w", err)
	}
	path := filepath.Join(l.dir, name)
	if err := os.Mkdir(path, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%w: %s", ErrFolderExists, name)
		}
		return "", fmt.Errorf("github: create folder: %w", err)
	}
	// Resolved, as the project list carries it, so a client finds the new folder there.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path, nil
}

// clonedPath is the first of the keys that is on disk.
func clonedPath(cloned map[string]string, keys ...string) string {
	for _, key := range keys {
		if path := cloned[strings.ToLower(key)]; path != "" {
			return path
		}
	}
	return ""
}

// clonedPaths indexes what is on disk, keyed by `owner/name` for a clone and
// by bare name for a loose folder. Lowercased because GitHub names are
// case-insensitive and the filesystem is not.
func (l Layout) clonedPaths() map[string]string {
	out := map[string]string{}
	loose, _ := childDirs(l.dir)
	for _, dir := range loose {
		out[strings.ToLower(filepath.Base(dir))] = dir
	}
	clones, _ := l.clones()
	for _, clone := range clones {
		owner := filepath.Base(filepath.Dir(clone))
		out[strings.ToLower(owner+"/"+filepath.Base(clone))] = clone
	}
	return out
}
