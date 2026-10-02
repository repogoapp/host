package projectwatch

import (
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/git"
)

// The events a watcher sends. Declared for the catalog; the watcher marshals
// once and sends the bytes itself, because a watch here is a lease.

func init() {
	emit.Register(Changed{}, Change{})
}

// Changed is a project's git status after something moved: a git.status row
// plus the working-tree totals, so the change chip needs no second call.
type Changed struct {
	Projects []Project `json:"projects" wire:"array"`
}

type Project struct {
	git.Status
	Working *git.ChangeSet `json:"working,omitempty"`
}

func (Changed) Method() string { return "git.changed" }

// Change is the files that moved under a watched folder, git repository or
// not. Only where there is a tree watcher (FSEvents): polling cannot tell
// which file changed.
type Change struct {
	// Path is the folder as the device watched it.
	Path string `json:"path"`
	// Changes are sorted by file.
	Changes []FileChange `json:"changes" wire:"array"`
	// Truncated is set when more files moved than one push lists: re-read
	// everything open.
	Truncated bool `json:"truncated,omitempty"`
}

func (Change) Method() string { return "fs.change" }

// FileChange is one file or folder, relative to the watched folder.
type FileChange struct {
	File string `json:"file"`
	// Kind is KindCreate, KindUpdate or KindDelete.
	Kind string `json:"kind"`
}

const (
	KindCreate = "create"
	KindUpdate = "update"
	KindDelete = "delete"
)
