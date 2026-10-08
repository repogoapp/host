package actions

import (
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/store"
)

func init() {
	emit.Register(Snapshot{})
}

// Snapshot is a project's actions and the runs they show: every running run
// and each action's newest ended one. Sent whole to every device on each
// start and end; a device keeps the highest revision.
type Snapshot struct {
	Path     string            `json:"path"`
	Revision int64             `json:"revision"`
	Actions  []Action          `json:"actions" wire:"array"`
	Runs     []store.ActionRun `json:"runs" wire:"array"`
}

func (Snapshot) Method() string { return "actions.changed" }
