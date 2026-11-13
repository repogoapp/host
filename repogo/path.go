package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/repogoapp/host/internal/apphome"
)

// The installers put the binary in ~/.repogo/bin, which isn't on PATH;
// installing links it from ~/.local/bin so `repogo <command>` works, and adds
// ~/.local/bin to the shell's profile when PATH lacks it (a fresh Mac does).

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

// linkOnPath links repogo onto PATH and, when ~/.local/bin isn't on PATH,
// adds it to the shell's profile once. A file there that isn't a link is
// someone else's: left alone, and the profile with it.
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
	if got, err := os.Readlink(link); err != nil || got != target {
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
	}
	if onPath(filepath.Dir(link)) {
		return
	}
	profile, line, ok := shellProfile()
	if !ok {
		fmt.Fprintf(w, "To run repogo by name, add %s to your PATH:\n  export PATH=\"$HOME/.local/bin:$PATH\"\n", filepath.Dir(link))
		return
	}
	added, err := addToProfile(profile, line)
	switch {
	case err != nil:
		fmt.Fprintf(w, "Couldn't add %s to PATH in %s (%v). Add it yourself:\n  %s\n", filepath.Dir(link), tilde(profile), err, line)
	case added:
		fmt.Fprintf(w, "Added ~/.local/bin to PATH in %s. Open a new terminal to run repogo by name; until then use %s\n", tilde(profile), tilde(target))
	}
}

// shellProfile is the file the user's shell reads at startup, and the line
// that puts ~/.local/bin on PATH there. Not on Windows.
func shellProfile() (profile, line string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil || runtime.GOOS == "windows" {
		return "", "", false
	}
	line = `export PATH="$HOME/.local/bin:$PATH"`
	switch filepath.Base(os.Getenv("SHELL")) {
	case "zsh":
		dir := os.Getenv("ZDOTDIR")
		if dir == "" {
			dir = home
		}
		return filepath.Join(dir, ".zshrc"), line, true
	case "bash":
		// macOS terminals open login shells, which read .bash_profile only.
		if runtime.GOOS == "darwin" {
			return filepath.Join(home, ".bash_profile"), line, true
		}
		return filepath.Join(home, ".bashrc"), line, true
	case "fish":
		return filepath.Join(home, ".config", "fish", "conf.d", "repogo.fish"), "fish_add_path $HOME/.local/bin", true
	default:
		return filepath.Join(home, ".profile"), line, true
	}
}

// addToProfile appends line to profile unless it already names
// ~/.local/bin, reporting whether it wrote.
func addToProfile(profile, line string) (bool, error) {
	data, err := os.ReadFile(profile)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if strings.Contains(string(data), ".local/bin") {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(profile), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(profile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	block := "# Added by RepoGo, so `repogo` runs by name.\n" + line + "\n"
	if len(data) > 0 {
		block = "\n" + block
		if !strings.HasSuffix(string(data), "\n") {
			block = "\n" + block
		}
	}
	if _, err := f.WriteString(block); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}

// tilde writes a path under the home folder as ~/….
func tilde(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return path
}

// unlinkFromPath removes the link linkOnPath made, and nothing else. The
// profile line stays: ~/.local/bin is a common folder other tools use too.
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
