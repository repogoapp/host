//go:build unix

package clitool

import "path/filepath"

// packageLink is ~/.local/bin/<command>, the symlink to an installed package's
// entry, and the file it named before.
func packageLink(bin, command string, _ Package) (link, previous string) {
	link = filepath.Join(bin, command)
	previous, _ = filepath.EvalSymlinks(link)
	return link, previous
}

// linkPackage points link at p's entry.
func linkPackage(link string, p Package) error {
	return relink(link, filepath.Join(p.Dir, p.Entry))
}
