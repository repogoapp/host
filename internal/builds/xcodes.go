package builds

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Xcode is one installed Xcode. Selected is the one xcode-select names, which
// a build uses when it names none.
type Xcode struct {
	Path     string `json:"path"`
	Version  string `json:"version"`
	Build    string `json:"build"`
	Selected bool   `json:"selected"`
}

// Where Xcodes are looked for. Variables so a test can put fakes in their place.
var (
	mdfind       = "/usr/bin/mdfind"
	xcodeSelect  = "/usr/bin/xcode-select"
	applications = "/Applications"
)

// Xcodes lists the installed Xcodes, the selected one first. Spotlight finds
// one kept outside /Applications; /Applications is read too for a Mac with
// Spotlight off.
func (s *Service) Xcodes(ctx context.Context) ([]Xcode, error) {
	if !IOSSupported() {
		return nil, ErrUnsupported
	}
	paths := map[string]bool{}
	if out, err := exec.CommandContext(ctx, mdfind, "kMDItemCFBundleIdentifier == 'com.apple.dt.Xcode'").Output(); err == nil {
		for _, path := range strings.Split(string(out), "\n") {
			if path != "" && !strings.Contains(path, "/.Trash/") {
				paths[path] = true
			}
		}
	}
	found, _ := filepath.Glob(filepath.Join(applications, "Xcode*.app"))
	for _, path := range found {
		paths[path] = true
	}
	selected := ""
	if out, err := exec.CommandContext(ctx, xcodeSelect, "-p").Output(); err == nil {
		selected = strings.TrimSpace(string(out))
	}
	xcodes := []Xcode{}
	for path := range paths {
		x, ok := xcodeVersion(ctx, path)
		if !ok {
			continue
		}
		x.Selected = developerDir(path) == selected
		xcodes = append(xcodes, x)
	}
	sort.Slice(xcodes, func(i, j int) bool {
		if xcodes[i].Selected != xcodes[j].Selected {
			return xcodes[i].Selected
		}
		return xcodes[i].Path < xcodes[j].Path
	})
	return xcodes, nil
}

// xcodeVersion reads an Xcode's Contents/version.plist; false when path is
// not an Xcode.
func xcodeVersion(ctx context.Context, path string) (Xcode, bool) {
	out, err := exec.CommandContext(ctx, plutil, "-convert", "json", "-o", "-",
		filepath.Join(path, "Contents", "version.plist")).Output()
	if err != nil {
		return Xcode{}, false
	}
	var v struct {
		Version string `json:"CFBundleShortVersionString"`
		Build   string `json:"ProductBuildVersion"`
	}
	if json.Unmarshal(out, &v) != nil || v.Version == "" {
		return Xcode{}, false
	}
	return Xcode{Path: path, Version: v.Version, Build: v.Build}, true
}

// checkXcode accepts an empty path, the selected Xcode, or one Xcodes lists,
// so DEVELOPER_DIR never points at an arbitrary folder.
func (s *Service) checkXcode(path string) error {
	if path == "" {
		return nil
	}
	xcodes, err := s.Xcodes(s.ctx)
	if err != nil {
		return err
	}
	for _, x := range xcodes {
		if x.Path == filepath.Clean(path) {
			return nil
		}
	}
	return fmt.Errorf("%w: xcode %q is not an installed Xcode", ErrInvalid, path)
}

// developerDir is what DEVELOPER_DIR names for an Xcode.
func developerDir(xcode string) string {
	return filepath.Join(xcode, "Contents", "Developer")
}

// withXcode runs cmd with the given Xcode; empty keeps the selected one.
// /usr/bin/xcodebuild reads DEVELOPER_DIR, so no global setting changes.
func withXcode(cmd *exec.Cmd, xcode string) *exec.Cmd {
	if xcode != "" {
		cmd.Env = append(cmd.Env, "DEVELOPER_DIR="+developerDir(xcode))
	}
	return cmd
}
