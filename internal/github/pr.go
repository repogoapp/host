// Pull requests: how a chat ends. The smallest pair that closes the loop, open
// one and merge one, using the user's own `gh` credential.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/git"
)

var (
	ErrNoPullRequest = errkind.New(errkind.NotFound, "github: no pull request for this branch")
	ErrBadRef        = errkind.New(errkind.Invalid, "github: branch name is not usable")
	// A missing title would make gh open an editor.
	ErrNoTitle = errkind.New(errkind.Invalid, "github: pull request title is empty")

	// safeRef guards the one user-chosen string that reaches `gh` as an argument:
	// a leading dash would be read as a flag and `..` reaches for a non-branch.
	safeRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

	// mergeMethods is the closed set `gh pr merge` accepts. A caller names one
	// of three, never a flag.
	mergeMethods = map[string]string{
		"squash": "--squash",
		"merge":  "--merge",
		"rebase": "--rebase",
	}
)

// PullRequest is one PR, trimmed to what a phone shows and what a follow-up
// call needs.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Base   string `json:"base"`
	Head   string `json:"head"`
	Merged bool   `json:"merged,omitempty"`

	// Existed reports that the branch already had an open PR and nothing was
	// created. Opening one twice from a phone is a double tap, the same as
	// cloning twice.
	Existed bool `json:"existed,omitempty"`

	// Shipped is what CreatePR's push sent, existing PR or not.
	Shipped *git.Shipped `json:"shipped,omitempty"`
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

	_, shipped, err := s.git.Push(ctx, dir, branch)
	if err != nil {
		return PullRequest{}, err
	}

	// An open PR for this branch is the answer to "open a PR for this branch".
	if existing, err := s.pullRequest(ctx, dir, 0); err == nil && !existing.Merged {
		existing.Existed, existing.Shipped = true, shipped
		return existing, nil
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

	// Read it back rather than parsing the URL `gh` prints. The number is what
	// every later call takes, and a second cheap command beats a regex over
	// output that is meant for a terminal.
	created, err := s.pullRequest(ctx, dir, 0)
	created.Shipped = shipped
	return created, err
}

func (s *Service) pullRequest(ctx context.Context, dir string, number int) (PullRequest, error) {
	args := []string{"pr", "view"}
	if number > 0 {
		args = append(args, strconv.Itoa(number))
	}
	args = append(args, "--json", "number,url,state,title,baseRefName,headRefName,mergedAt")

	out, err := s.runIn(ctx, dir, commandTimeout, args...)
	if err != nil {
		// gh says this when the branch has no PR, which is an ordinary answer
		// and not a failure — every chat has this state until it ships.
		if strings.Contains(err.Error(), "no pull requests found") ||
			strings.Contains(err.Error(), "no open pull requests") {
			return PullRequest{}, ErrNoPullRequest
		}
		return PullRequest{}, err
	}

	var raw struct {
		Number      int    `json:"number"`
		URL         string `json:"url"`
		State       string `json:"state"`
		Title       string `json:"title"`
		BaseRefName string `json:"baseRefName"`
		HeadRefName string `json:"headRefName"`
		MergedAt    string `json:"mergedAt"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return PullRequest{}, fmt.Errorf("github: unreadable pull request: %w", err)
	}
	return PullRequest{
		Number: raw.Number,
		URL:    raw.URL,
		State:  raw.State,
		Title:  raw.Title,
		Base:   raw.BaseRefName,
		Head:   raw.HeadRefName,
		Merged: raw.MergedAt != "",
	}, nil
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
