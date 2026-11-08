package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/repogoapp/host/internal/apphome"
)

// The installers put the binary in ~/.repogo/bin, which isn't on PATH;
// installing links it from ~/.local/bin so `repogo <command>` works.

// pathLink is the link: ~/.local/bin/repogo.
func pathLink() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin", "repogo"), nil
}

// linkTarget is the installed binary the link points at.
func linkTarget() (string, error) { return apphome.Path("bin", "repogo") }

// linkOnPath links repogo onto PATH, and, when it makes the link, says how to
// add ~/.local/bin to PATH if it isn't. A file there that isn't a link is
// someone else's: left alone.
func linkOnPath(w io.Writer) {
	link, err := pathLink()
	if err != nil {
		return
	}
	target, err := linkTarget()
	if err != nil {
		return
	}
	if _, err := os.Stat(target); err != nil {
		return
	}
	if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return
	}
	if got, err := os.Readlink(link); err == nil && got == target {
		return
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return
	}
	// A new link swapped into place, so the old one is never missing.
	tmp := link + ".new"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return
	}
	if !onPath(filepath.Dir(link)) {
		fmt.Fprintf(w, "To run repogo by name, add %s to your PATH:\n  export PATH=\"$HOME/.local/bin:$PATH\"\n", filepath.Dir(link))
	}
}

// unlinkFromPath removes the link linkOnPath made, and nothing else.
func unlinkFromPath() {
	link, err := pathLink()
	if err != nil {
		return
	}
	target, err := linkTarget()
	if err != nil {
		return
	}
	if got, err := os.Readlink(link); err == nil && got == target {
		os.Remove(link)
	}
}

func onPath(dir string) bool {
	for _, d := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if filepath.Clean(d) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}
