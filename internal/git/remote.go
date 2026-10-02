package git

import (
	"context"
	"strings"
)

// Remote is `owner/repo` for one checkout. Separate from the status batch
// because every project needs it and it rarely changes; empty for a plain folder.
func (s *Service) Remote(ctx context.Context, path string) (string, error) {
	dir, err := s.paths.Contain(path)
	if err != nil {
		return "", err
	}
	return ParseRemote(origin(ctx, dir)), nil
}

// OwnerRepo is the checkout's origin as owner and name; both empty without one.
func OwnerRepo(ctx context.Context, dir string) (owner, name string) {
	owner, name, _ = strings.Cut(ParseRemote(origin(ctx, dir)), "/")
	return owner, name
}

func origin(ctx context.Context, dir string) string {
	url, _ := run(ctx, dir, "remote", "get-url", "origin")
	return url
}

// ParseRemote reduces `git@host:owner/repo.git`, `https://host/owner/repo.git`
// or `ssh://git@host/owner/repo.git` to `owner/repo`. Host-agnostic so GitLab
// and self-hosted forges name a project the same way; empty when it cannot.
func ParseRemote(url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return ""
	}
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimSuffix(url, "/")

	// Cut the scheme and any credentials, then the host. `scp`-style URLs use a
	// colon where a URL uses a slash, so both separators are tried.
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
	}
	if i := strings.LastIndex(url, "@"); i >= 0 {
		url = url[i+1:]
	}
	if i := strings.Index(url, ":"); i >= 0 {
		url = url[i+1:]
	} else if i := strings.Index(url, "/"); i >= 0 {
		url = url[i+1:]
	}

	parts := strings.Split(strings.Trim(url, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	owner, repo := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || repo == "" {
		return ""
	}
	return owner + "/" + repo
}
