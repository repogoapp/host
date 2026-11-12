package clitool

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/repogoapp/host/internal/binfetch"
)

// fetchable reports whether the CLI has a release build to fetch.
func (s Spec) fetchable() bool { return s.Package != nil || s.Release != nil }

// fetchBuild installs the CLI's release: its whole package when it has one,
// else its one binary. It answers the URL fetched, for the log.
func fetchBuild(ctx context.Context, s Spec) (string, error) {
	if s.Package == nil {
		url, member, err := s.Release(ctx, runtime.GOOS, runtime.GOARCH)
		if err != nil {
			return url, err
		}
		name := s.Command
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		_, err = binfetch.Install(ctx, url, member, name)
		return url, err
	}
	p, err := s.Package(ctx, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return p.URL, err
	}
	bin, err := binfetch.Dir()
	if err != nil {
		return p.URL, err
	}
	link := filepath.Join(bin, s.Command)
	previous, _ := filepath.EvalSymlinks(link)
	if err := binfetch.InstallPackage(ctx, p.URL, p.SHA256, p.Dir); err != nil {
		return p.URL, err
	}
	if err := relink(link, filepath.Join(p.Dir, p.Entry)); err != nil {
		return p.URL, err
	}
	prune(filepath.Dir(p.Dir), p.Dir, previous)
	return p.URL, nil
}

// relink points link at target in one rename, replacing the binary or link
// there: a running copy keeps its own, and nothing sees the name missing.
func relink(link, target string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	tmp := link + ".new"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// prune removes the older versions under parent, keeping current and the one
// the link named before, which a running copy may still spawn helpers from.
func prune(parent, current, previous string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(parent, e.Name())
		if !e.IsDir() || dir == current || strings.HasPrefix(e.Name(), ".") ||
			(previous != "" && strings.HasPrefix(previous, dir+string(filepath.Separator))) {
			continue
		}
		os.RemoveAll(dir)
	}
}
