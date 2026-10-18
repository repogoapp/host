package actions

import (
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/store"
)

func init() {
	emit.Register(Snapshot{})
}

// Snapshot is a project's actions and the runs they show: every running run
// and each action's newest ended one. Sent whole to every device on each
// start and end; a device keeps the highest revision.
type Snapshot struct {
	Path     string   `json:"path"`
	Revision int64    `json:"revision"`
	Actions  []Action `json:"actions" wire:"array"`
	Runs     []Run    `json:"runs" wire:"array"`
}

func (Snapshot) Method() string { return "actions.changed" }

// Run is one run as the phone shows it: whether it is going, how it is held,
// and how it ended.
type Run struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	Mode      string `json:"mode"`
	Status    string `json:"status"`
	StartedAt int64  `json:"started_at"`
	// Zero while running, and for a lost run.
	EndedAt  int64 `json:"ended_at"`
	ExitCode *int  `json:"exit_code,omitempty"`
}

func runOf(r store.ActionRun) Run {
	return Run{ID: r.ID, Action: r.Action, Mode: r.Mode, Status: r.Status, StartedAt: r.StartedAt, EndedAt: r.EndedAt, ExitCode: r.ExitCode}
}
