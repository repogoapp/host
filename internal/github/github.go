// Package github lists the user's repositories and clones one into a project,
// via `gh`: the user signed in on this machine once and the host borrows that
// sign-in rather than ever issuing one.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/git"
)

const (
	// A list or an auth check is one HTTPS round trip. Long enough for a slow
	// network, short enough that a phone gets an answer rather than a spinner.
	commandTimeout = 20 * time.Second

	// A clone is a real download and a large repo genuinely takes minutes.
	cloneTimeout = 10 * time.Minute

	// reposDir holds `<dir>/.repos/<owner>/<repo>` clones: the repository's
	// folder is the checkout, with no branch in the path, so switching branches
	// in it leaves nothing stale. Owner-scoped because repo names collide.
	reposDir = ".repos"

	// MaxRepos bounds one listing. Nobody scrolls past this on a phone, and the
	// filtering that finds the repo you want happens on the string, not by
	// fetching more pages.
	MaxRepos = 200
)

var (
	ErrNotInstalled = errkind.New(errkind.Unavailable, "github: the gh CLI is not installed")
	ErrNotSignedIn  = errkind.New(errkind.Denied, "github: gh is installed but not signed in")
	ErrBadName      = errkind.New(errkind.Invalid, "github: repository must be owner/name")
	ErrBadFolder    = errkind.New(errkind.Invalid, "github: folder name must be letters, numbers, '.', '_' or '-', not starting with '.'")
	ErrFolderExists = errkind.New(errkind.Invalid, "github: a project with that name already exists")
)

// safeName is the whole defence for the clone destination: anything that is not
// a plain owner/name is refused rather than sanitised.
var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// CloneFolder is where this host clones repositories, shown so the answer to
// "where did it go" is on screen instead of in our source.
type CloneFolder struct {
	// Dir holds clones as `<owner>/<repo>`, so a client can say where one
	// will go before it starts.
	Dir string `json:"dir"`
}

// Repo is one repository, trimmed to what a picker shows.
type Repo struct {
	NameWithOwner string `json:"name_with_owner"`
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	Description   string `json:"description,omitempty"`
	Private       bool   `json:"private,omitempty"`

	// Path is set when this repo is already cloned here, which turns the picker
	// into a project list: the rows you have and the rows you could have, in
	// one place, rather than a list that offers to clone what you already have.
	Path string `json:"path,omitempty"`
}

// Clone is where a repository ended up.
type Clone struct {
	Path string `json:"path"`

	// Existed reports that the folder was already there and nothing was
	// downloaded. Cloning twice from a phone is a double tap, not an error.
	Existed bool `json:"existed,omitempty"`
}

// Service clones into one directory and serves that directory's children as
// projects. Paths a client names are contained by git before gh runs in them.
type Service struct {
	Layout
	git *git.Service
	log *slog.Logger
}

// New takes the directory clones land in, created on first clone.
func New(layout Layout, g *git.Service, log *slog.Logger) *Service {
	return &Service{Layout: layout, git: g, log: log}
}

// CloneFolder is where clones land.
func (s *Service) CloneFolder() CloneFolder { return CloneFolder{Dir: filepath.Join(s.dir, reposDir)} }

// Repos lists repositories, newest push first. `owner` defaults to the
// signed-in account, counting repos it collaborates on and its organizations'
// as well as its own; `query` filters the returned page rather than issuing a
// search, which would need its own failure handling.
func (s *Service) Repos(ctx context.Context, owner, query string, limit int) ([]Repo, error) {
	if limit <= 0 || limit > MaxRepos {
		limit = MaxRepos
	}
	var raw []listedRepo
	var err error
	if owner == "" {
		raw, err = s.viewerRepos(ctx, limit)
	} else {
		raw, err = s.ownerRepos(ctx, owner, limit)
	}
	if err != nil {
		return nil, err
	}
	// GitHub's ordering differs per endpoint; recently pushed is the one that
	// puts what you are working on at the top.
	sort.SliceStable(raw, func(i, j int) bool { return raw[i].PushedAt > raw[j].PushedAt })

	cloned := s.clonedPaths()
	needle := strings.ToLower(strings.TrimSpace(query))
	repos := make([]Repo, 0, len(raw))
	for _, r := range raw {
		if needle != "" &&
			!strings.Contains(strings.ToLower(r.NameWithOwner), needle) &&
			!strings.Contains(strings.ToLower(r.Description), needle) {
			continue
		}
		repos = append(repos, Repo{
			NameWithOwner: r.NameWithOwner,
			Name:          r.Name,
			Owner:         r.Owner,
			Description:   r.Description,
			Private:       r.Private,
			// `owner/name` first, and only then the bare name: a folder the
			// user put in the clone directory by hand has no owner to check
			// against, so it can only be matched on the name it has.
			Path: clonedPath(cloned, r.NameWithOwner, r.Name),
		})
	}
	return repos, nil
}

// listedRepo is one repository as either listing returns it.
type listedRepo struct {
	NameWithOwner string
	Name          string
	Owner         string
	Description   string
	Private       bool
	PushedAt      string
}

// reposPage is GitHub's own page size cap for `/user/repos`.
const reposPage = 100

// viewerRepos uses the REST API to include collaborator and organization repos.
// Pages are fetched one at a time so large accounts stop at the limit instead
// of downloading everything.
func (s *Service) viewerRepos(ctx context.Context, limit int) ([]listedRepo, error) {
	var repos []listedRepo
	for page := 1; len(repos) < limit; page++ {
		path := fmt.Sprintf("/user/repos?affiliation=owner,collaborator,organization_member&sort=pushed&per_page=%d&page=%d", reposPage, page)
		out, err := s.run(ctx, commandTimeout, "api", path)
		if err != nil {
			return nil, err
		}
		var raw []struct {
			FullName string `json:"full_name"`
			Name     string `json:"name"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
			Description string `json:"description"`
			Private     bool   `json:"private"`
			PushedAt    string `json:"pushed_at"`
		}
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			return nil, fmt.Errorf("github: unreadable repo list: %w", err)
		}
		for _, r := range raw {
			repos = append(repos, listedRepo{r.FullName, r.Name, r.Owner.Login, r.Description, r.Private, r.PushedAt})
		}
		if len(raw) < reposPage {
			break
		}
	}
	if len(repos) > limit {
		repos = repos[:limit]
	}
	return repos, nil
}

// ownerRepos lists one user's or organization's repositories, which `gh repo
// list` resolves whichever the login is.
func (s *Service) ownerRepos(ctx context.Context, owner string, limit int) ([]listedRepo, error) {
	if strings.ContainsAny(owner, "/ \t") || strings.HasPrefix(owner, "-") {
		return nil, fmt.Errorf("%w: %q is not an owner", ErrBadName, owner)
	}
	// No `defaultBranchRef`: GitHub resolves it per repository, which tripled
	// the call (about 2 s to 5 s for 130 repos). A checkout reads its own
	// default from `origin/HEAD` (git.branches) in milliseconds.
	out, err := s.run(ctx, commandTimeout, "repo", "list", owner, "--limit", fmt.Sprint(limit),
		"--json", "nameWithOwner,name,owner,description,isPrivate,pushedAt")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		NameWithOwner string `json:"nameWithOwner"`
		Name          string `json:"name"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
		Description string `json:"description"`
		IsPrivate   bool   `json:"isPrivate"`
		PushedAt    string `json:"pushedAt"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("github: unreadable repo list: %w", err)
	}
	repos := make([]listedRepo, len(raw))
	for i, r := range raw {
		repos[i] = listedRepo{r.NameWithOwner, r.Name, r.Owner.Login, r.Description, r.IsPrivate, r.PushedAt}
	}
	return repos, nil
}

// run executes one gh command, from wherever the daemon happens to be.
func (s *Service) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	return s.runIn(ctx, "", timeout, args...)
}

// runIn executes one gh command inside a project; the directory is how `gh`
// knows which repository it is talking about.
func (s *Service) runIn(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", ErrNotInstalled
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	// gh will open a browser to sign in and git will ask for a password, and
	// neither has anywhere to happen on a headless daemon answering a phone —
	// they would just hold the request open until it times out.
	cmd.Env = append(cmd.Environ(),
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GIT_TERMINAL_PROMPT=0",
	)

	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		// gh says this on every command when the token is missing or expired,
		// and it is the one failure with an obvious fix. Reported as its own
		// error so a client can say "sign in" instead of showing gh's prose.
		if strings.Contains(msg, "gh auth login") || strings.Contains(msg, "authentication token") {
			return "", ErrNotSignedIn
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", args[0], msg)
	}
	return stdout.String(), nil
}
