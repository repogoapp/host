package clitool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// A Windows symlink needs admin rights, which the host's task lacks, and a
// hard link won't do: Codex finds its helpers beside its own exe. So the whole
// package is linked by a junction, which any user may make, beside the
// versions (~/.local/share/codex-current); the tool's NativePaths name it.

// packageLink is that junction, and the version folder it named before.
func packageLink(_, _ string, p Package) (link, previous string) {
	link = filepath.Dir(p.Dir) + "-current"
	previous, _ = os.Readlink(link)
	return link, previous
}

// linkPackage points the junction at p.Dir. os.Remove takes away a junction,
// never what it points at.
func linkPackage(link string, p Package) error {
	tmp := link + ".new"
	os.Remove(tmp)
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", tmp, p.Dir).CombinedOutput(); err != nil {
		return fmt.Errorf("clitool: junction to %s: %w: %s", p.Dir, err, strings.TrimSpace(string(out)))
	}
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
