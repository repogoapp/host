// Publishing: a folder with no GitHub repository gets one, created by the
// host's own `gh` sign-in, so a phone never holds a GitHub identity.
package github

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/repogo/host/internal/errkind"
)

var (
	ErrBadRepoName = errkind.New(errkind.Invalid, "github: repository name must be letters, numbers, '.', '_' or '-'")

	// repoName is a bare name: the owner is always the signed-in account.
	repoName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// Published is the repository a folder now points at. Nothing is pushed: the
// client commits and pushes with `git.commit_push`, as for any other change.
type Published struct {
	NameWithOwner string `json:"name_with_owner"`
	URL           string `json:"url"`
	Branch        string `json:"branch"`
}

// Publish creates `name` on GitHub under the signed-in account and makes it
// the folder's origin, initialising git first when the folder is not a
// repository of its own.
func (s *Service) Publish(ctx context.Context, path, name string, private bool) (Published, error) {
	// A leading '-' would reach gh as a flag.
	if !repoName.MatchString(name) || name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return Published{}, fmt.Errorf("%w: %q", ErrBadRepoName, name)
	}
	dir, err := s.git.Contain(path)
	if err != nil {
		return Published{}, err
	}
	if err = s.git.PrepareOrigin(ctx, dir, "main"); err != nil {
		return Published{}, err
	}
	branch, err := currentBranch(ctx, dir)
	if err != nil {
		return Published{}, err
	}

	visibility := "--private"
	if !private {
		visibility = "--public"
	}
	out, err := s.runIn(ctx, dir, commandTimeout,
		"repo", "create", name, visibility, "--source", dir, "--remote", "origin")
	if err != nil {
		return Published{}, err
	}
	return published(out, branch)
}

// published reads the URL `gh repo create` prints on its last line.
func published(out, branch string) (Published, error) {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return Published{}, fmt.Errorf("github: gh repo create printed no URL")
	}
	link := fields[len(fields)-1]
	parsed, err := url.Parse(link)
	if err != nil {
		return Published{}, fmt.Errorf("github: unexpected gh repo create output %q", link)
	}
	path := strings.Trim(parsed.Path, "/")
	if parsed.Host == "" || strings.Count(path, "/") != 1 {
		return Published{}, fmt.Errorf("github: unexpected gh repo create output %q", link)
	}
	return Published{NameWithOwner: path, URL: link, Branch: branch}, nil
}
