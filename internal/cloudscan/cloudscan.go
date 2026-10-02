// Package cloudscan finds the cloud projects configured under a folder:
// Vercel links and vercel.json, Fly's fly.toml and Cloudflare's wrangler
// config. It reads only files on disk and never calls a provider.
package cloudscan

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/git"
)

const (
	ProviderVercel     = "vercel"
	ProviderFly        = "fly"
	ProviderCloudflare = "cloudflare"
)

// Project is one project a config names. ID is the provider's name for it (a
// Vercel project id, a Fly app, a Worker), empty until the folder is linked;
// TeamID is the Vercel team or Cloudflare account when the file names one.
type Project struct {
	Provider   string `json:"provider"`
	Name       string `json:"name"`
	ID         string `json:"id"`
	TeamID     string `json:"team_id"`
	Directory  string `json:"directory"`
	ConfigPath string `json:"config_path"`
}

type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type Detection struct {
	Cwd      string    `json:"cwd"`
	Projects []Project `json:"projects" wire:"array"`
	Issues   []Issue   `json:"issues" wire:"array"`
}

type Scanner struct{ paths files.Container }

func New(paths files.Container) *Scanner {
	if paths == nil {
		panic("cloudscan: missing paths")
	}
	return &Scanner{paths: paths}
}

var ErrInvalidPath = errkind.New(errkind.Invalid, "cloudscan: cwd must name a directory")

// Detect lists the projects under path, one row per config. A vercel.json
// inside a linked folder belongs to that link, so it isn't a row of its own.
func (s *Scanner) Detect(ctx context.Context, path string) (Detection, error) {
	found, err := s.scan(ctx, path)
	if err != nil {
		return Detection{}, err
	}
	linked := []string{}
	for _, p := range found.Projects {
		if p.Provider == ProviderVercel && p.ID != "" {
			linked = append(linked, p.Directory)
		}
	}
	result := Detection{Cwd: found.Cwd, Projects: []Project{}, Issues: found.Issues}
	for _, p := range found.Projects {
		if p.Provider == ProviderVercel && p.ID == "" && within(linked, p.Directory) {
			continue
		}
		result.Projects = append(result.Projects, p)
	}
	return result, nil
}

func within(dirs []string, dir string) bool {
	for _, d := range dirs {
		if files.Within(d, dir) {
			return true
		}
	}
	return false
}

var skippedDirectoryNames = []string{".git", "node_modules", ".next", ".build", "Pods", "vendor", ".cache", ".turbo"}

// scan returns every config under path, sorted by directory then file. Inside
// a repository it reads git's listing, so ignored folders (build output,
// cloned references) are never walked.
func (s *Scanner) scan(ctx context.Context, path string) (Detection, error) {
	if path == "" {
		return Detection{}, ErrInvalidPath
	}
	root, err := s.paths.Contain(path)
	if err != nil {
		return Detection{}, err
	}
	scoped, err := os.OpenRoot(root)
	if err != nil {
		return Detection{}, fmt.Errorf("%w: %v", ErrInvalidPath, err)
	}
	defer scoped.Close()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	result := Detection{Cwd: root, Projects: []Project{}, Issues: []Issue{}}
	outside := found{scoped: scoped, root: root}
	repos := []string{}
	if git.Root(root) != "" {
		repos = append(repos, ".")
	} else if repos, err = outside.walk(ctx, skippedDirectoryNames); err != nil {
		return result, err
	}

	inside := make([]found, len(repos))
	var workers sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, repo := range repos {
		inside[i] = found{scoped: scoped, root: root}
		workers.Add(1)
		go func(f *found, repo string) {
			defer workers.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			f.repo(ctx, repo)
		}(&inside[i], repo)
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return result, err
	}

	for _, f := range append([]found{outside}, inside...) {
		result.Projects = append(result.Projects, f.projects...)
		result.Issues = append(result.Issues, f.issues...)
	}
	sort.SliceStable(result.Projects, func(i, j int) bool {
		a, b := result.Projects[i], result.Projects[j]
		if a.Directory != b.Directory {
			return a.Directory < b.Directory
		}
		return a.ConfigPath < b.ConfigPath
	})
	return result, nil
}

// found collects one part of a scan, so repositories can be read in parallel.
type found struct {
	scoped   *os.Root
	root     string
	projects []Project
	issues   []Issue
}

// configName reports whether a file is one this package reads.
func configName(name string) bool {
	switch name {
	case "vercel.json", "fly.toml", "wrangler.toml", "wrangler.json", "wrangler.jsonc":
		return true
	}
	return false
}

// walk reads the folders outside any repository and returns the repositories
// it meets, relative to the root, for git to list.
func (f *found) walk(ctx context.Context, skipped []string) ([]string, error) {
	skip := map[string]bool{}
	for _, name := range skipped {
		skip[name] = true
	}
	repos := []string{}
	visited := 0
	err := fs.WalkDir(f.scoped.FS(), ".", func(relative string, entry fs.DirEntry, walkErr error) error {
		visited++
		if visited > 500000 {
			f.issues = append(f.issues, Issue{Path: f.root, Message: "directory scan exceeded 500000 entries"})
			return fs.SkipAll
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			f.issues = append(f.issues, Issue{Path: filepath.Join(f.root, relative), Message: walkErr.Error()})
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		switch {
		case !entry.IsDir():
			if configName(entry.Name()) {
				f.config(relative)
			}
		case relative != "." && skip[entry.Name()]:
			return filepath.SkipDir
		case entry.Name() == ".vercel":
			f.linkFolder(relative)
			return filepath.SkipDir
		default:
			if _, err := f.scoped.Lstat(filepath.Join(relative, ".git")); err == nil {
				repos = append(repos, relative)
				return filepath.SkipDir
			}
		}
		return nil
	})
	return repos, err
}

// repo reads what git lists in a repository. An ignored .vercel still counts:
// `vercel link` adds it to .gitignore.
func (f *found) repo(ctx context.Context, dir string) {
	listing, err := git.List(ctx, filepath.Join(f.root, dir), []string{":(glob)**/.vercel"})
	if err != nil {
		f.issues = append(f.issues, Issue{Path: filepath.Join(f.root, dir), Message: err.Error()})
		return
	}
	for _, name := range listing.Files {
		relative := filepath.Join(dir, filepath.FromSlash(name))
		switch {
		case strings.HasSuffix(name, "/"):
			f.repo(ctx, relative)
		case isLinkFile(relative):
			f.link(relative)
		case configName(filepath.Base(relative)):
			if info, err := f.scoped.Lstat(relative); err == nil && info.Mode().IsRegular() {
				f.config(relative)
			}
		}
	}
	for _, name := range listing.Ignored {
		f.linkFolder(filepath.Join(dir, filepath.FromSlash(name)))
	}
}

func isLinkFile(relative string) bool {
	name := filepath.Base(relative)
	return filepath.Base(filepath.Dir(relative)) == ".vercel" && (name == "project.json" || name == "repo.json")
}

func (f *found) linkFolder(dir string) {
	for _, name := range []string{"project.json", "repo.json"} {
		f.link(filepath.Join(dir, name))
	}
}

// link reads a .vercel link file; a missing one is not an issue.
func (f *found) link(relative string) {
	source := filepath.Join(f.root, relative)
	info, err := f.scoped.Lstat(relative)
	if os.IsNotExist(err) {
		return
	}
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("link file must be regular")
	}
	if err == nil {
		var projects []Project
		if projects, err = readVercelLink(f.scoped, f.root, relative); err == nil {
			f.projects = append(f.projects, projects...)
			return
		}
	}
	f.issues = append(f.issues, Issue{Path: source, Message: err.Error()})
}

// config reads one provider config file into a project row.
func (f *found) config(relative string) {
	project := Project{Directory: filepath.Join(f.root, filepath.Dir(relative)), ConfigPath: filepath.Join(f.root, relative)}
	var err error
	switch name := filepath.Base(relative); name {
	case "vercel.json":
		project.Provider = ProviderVercel
	case "fly.toml":
		project.Provider = ProviderFly
		err = readFly(f.scoped, relative, &project)
	default:
		project.Provider = ProviderCloudflare
		err = readWrangler(f.scoped, relative, &project)
	}
	if err != nil {
		f.issues = append(f.issues, Issue{Path: project.ConfigPath, Message: err.Error()})
		return
	}
	f.projects = append(f.projects, project)
}
