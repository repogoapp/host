package projectsync

import (
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/store"
)

func init() {
	emit.Register(Changed{})
}

// Changed is the project rows a pass moved, whole, and the paths that are no
// longer projects; sent to every paired device, so a list stays in order
// without asking project.list again.
type Changed struct {
	Projects []store.Project `json:"projects" wire:"array"`
	Removed  []string        `json:"removed" wire:"array"`
}

func (Changed) Method() string { return "project.changed" }
