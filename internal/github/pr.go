// Pull requests: how a chat ends. The smallest pair that closes the loop, open
// one and merge one, using the user's own `gh` credential.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/git"
)

var (
	ErrNoPullRequest = errkind.New(errkind.NotFound, "github: no pull request for this branch")
	ErrBadRef        = errkind.New(errkind.Invalid, "github: branch name is not usable")
	// A missing title would make gh open an editor.
	ErrNoTitle = errkind.New(errkind.Invalid, "github: pull request title is empty")

	// safeRef guards the one user-chosen string that reaches `gh` as an argument:
	// a leading dash would be read as a flag and `..` reaches for a non-branch.
	safeRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

// PullRequest is the page a client opens once the pull request exists.
type PullRequest struct {
	URL string `json:"url"`
}

// NewPR is what opening one needs. Base empty means the repository's default
// branch, which is what `gh` picks on its own.
type NewPR struct {
	Title string
	Body  string
	Base  string
	Draft bool
}

// CreatePR pushes the current branch and opens a pull request for it in one
// action: GitHub cannot open a PR for a branch it has never seen.
func (s *Service) CreatePR(ctx context.Context, path string, pr NewPR) (PullRequest, error) {
	if strings.TrimSpace(pr.Title) == "" {
		return PullRequest{}, ErrNoTitle
	}
	dir, err := s.git.Contain(path)
	if err != nil {
		return PullRequest{}, err
	}
	branch, err := currentBranch(ctx, dir)
	if err != nil {
		return PullRequest{}, err
	}
	if pr.Base != "" && !safeRef.MatchString(pr.Base) {
		return PullRequest{}, fmt.Errorf("%w: %q", ErrBadRef, pr.Base)
	}

	if err := s.git.Push(ctx, dir, branch); err != nil {
		return PullRequest{}, err
	}

	// An open PR for this branch is the answer to "open a PR for this branch":
	// opening one twice from a phone is a double tap.
	if url, merged, err := s.pullRequest(ctx, dir); err == nil && !merged {
		return PullRequest{URL: url}, nil
	}

	// `--flag=value`, so no client text is ever read as a flag of its own.
	args := []string{"pr", "create", "--head=" + branch, "--title=" + pr.Title, "--body=" + pr.Body}
	if pr.Base != "" {
		args = append(args, "--base="+pr.Base)
	}
	if pr.Draft {
		args = append(args, "--draft")
	}
	if _, err := s.runIn(ctx, dir, commandTimeout, args...); err != nil {
		return PullRequest{}, err
	}

	// Read it back rather than parsing the URL `gh` prints: a second cheap
	// command beats a regex over output that is meant for a terminal.
	url, _, err := s.pullRequest(ctx, dir)
	return PullRequest{URL: url}, err
}

// pullRequest is the checked-out branch's pull request: its page, and whether
// it is merged.
func (s *Service) pullRequest(ctx context.Context, dir string) (url string, merged bool, err error) {
	out, err := s.runIn(ctx, dir, commandTimeout, "pr", "view", "--json", "url,mergedAt")
	if err != nil {
		// gh says this when the branch has no PR, which is an ordinary answer
		// and not a failure — every chat has this state until it ships.
		if strings.Contains(err.Error(), "no pull requests found") ||
			strings.Contains(err.Error(), "no open pull requests") {
			return "", false, ErrNoPullRequest
		}
		return "", false, err
	}

	var raw struct {
		URL      string `json:"url"`
		MergedAt string `json:"mergedAt"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return "", false, fmt.Errorf("github: unreadable pull request: %w", err)
	}
	return raw.URL, raw.MergedAt != "", nil
}

// currentBranch is the checkout's branch, refused unless it is safe to hand `gh`.
func currentBranch(ctx context.Context, dir string) (string, error) {
	branch, err := git.CurrentBranch(ctx, dir)
	if err != nil {
		return "", err
	}
	if !safeRef.MatchString(branch) {
		return "", fmt.Errorf("%w: %q", ErrBadRef, branch)
	}
	return branch, nil
}
