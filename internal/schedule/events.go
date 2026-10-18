package schedule

import (
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/store"
)

func init() {
	emit.Register(Changed{})
}

// Schedule is a schedule as schedules.list sends it: what the user set, the
// host it runs on, and when it runs next.
type Schedule struct {
	store.Schedule
	HostID string `json:"host_id"`
	// Milliseconds; 0 when it is off or has no time left to run.
	NextRunAt int64 `json:"next_run_at"`
}

// Changed is every schedule on this host, sent to every paired device after
// a save, a delete, or a run, so each list and its next times stay current.
type Changed struct {
	Schedules []Schedule `json:"schedules" wire:"array"`
}

func (Changed) Method() string { return "schedules.changed" }
