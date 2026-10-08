package store

import (
	"database/sql"
	"fmt"
)

// Action run statuses. Running is the only one that changes.
const (
	RunRunning  = "running"
	RunExited   = "exited"
	RunStopped  = "stopped"
	RunTimedOut = "timed_out"
	RunLost     = "lost"
)

// ActionRun is one run of a project's action, as kept and as sent.
type ActionRun struct {
	ID        string `json:"id"`
	Path      string `json:"-"`
	Action    string `json:"action"`
	Cmd       string `json:"cmd"`
	Mode      string `json:"mode"` // blocking | detached | terminal
	DeviceID  string `json:"device_id"`
	PID       int    `json:"pid"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at"` // 0 while running, and for a lost run
	ExitCode  *int   `json:"exit_code,omitempty"`
	// PIDStarted is the process's own start time, ms, so a reused pid is
	// never taken for this run after a restart.
	PIDStarted int64 `json:"-"`
}

const actionRunColumns = `id, path, action, cmd, mode, device_id, pid, pid_started, session_id,
	status, started_at, ended_at, exit_code`

// InsertActionRun keeps a run that has just started.
func (s *Store) InsertActionRun(r ActionRun) error {
	_, err := s.db.Exec(`INSERT INTO state.action_runs (`+actionRunColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)`,
		r.ID, r.Path, r.Action, r.Cmd, r.Mode, r.DeviceID, r.PID, r.PIDStarted, r.SessionID,
		RunRunning, r.StartedAt)
	if err != nil {
		return fmt.Errorf("store: insert action run: %w", err)
	}
	return nil
}

// SettleActionRun ends a running run. False when it had ended already, so
// two ways of noticing one end write it once.
func (s *Store) SettleActionRun(id, status string, endedAt int64, exitCode *int) (bool, error) {
	var ended sql.NullInt64
	if endedAt > 0 {
		ended = sql.NullInt64{Int64: endedAt, Valid: true}
	}
	result, err := s.db.Exec(`UPDATE state.action_runs SET status = ?, ended_at = ?, exit_code = ?
		WHERE id = ? AND status = ?`, status, ended, exitCode, id, RunRunning)
	if err != nil {
		return false, fmt.Errorf("store: settle action run: %w", err)
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ActionRun is one run by id; ok is false when there is none.
func (s *Store) ActionRun(id string) (ActionRun, bool, error) {
	runs, err := s.queryActionRuns(`SELECT `+actionRunColumns+` FROM state.action_runs WHERE id = ?`, id)
	if err != nil || len(runs) == 0 {
		return ActionRun{}, false, err
	}
	return runs[0], true, nil
}

// ActionRuns is what a project's actions show: every running run, and each
// action's newest ended one.
func (s *Store) ActionRuns(path string) ([]ActionRun, error) {
	return s.queryActionRuns(`SELECT `+actionRunColumns+` FROM state.action_runs r
		WHERE path = ? AND (status = ? OR id = (SELECT id FROM state.action_runs n
			WHERE n.path = r.path AND n.action = r.action AND n.status != ?
			ORDER BY started_at DESC, id DESC LIMIT 1))
		ORDER BY started_at, id`, path, RunRunning, RunRunning)
}

// RunningActionRuns is every run still marked running, for a host starting up.
func (s *Store) RunningActionRuns() ([]ActionRun, error) {
	return s.queryActionRuns(`SELECT `+actionRunColumns+` FROM state.action_runs WHERE status = ?
		ORDER BY started_at, id`, RunRunning)
}

// PruneActionRuns keeps an action's newest keep ended runs and returns the
// ids it deleted, whose logs go with them. A running run is never pruned.
func (s *Store) PruneActionRuns(path, action string, keep int) ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM state.action_runs WHERE path = ? AND action = ? AND status != ?
		ORDER BY started_at DESC, id DESC LIMIT -1 OFFSET ?`, path, action, RunRunning, keep)
	if err != nil {
		return nil, fmt.Errorf("store: prune action runs: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := s.db.Exec(`DELETE FROM state.action_runs WHERE id = ?`, id); err != nil {
			return nil, fmt.Errorf("store: prune action runs: %w", err)
		}
	}
	return ids, nil
}

func (s *Store) queryActionRuns(query string, args ...any) ([]ActionRun, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read action runs: %w", err)
	}
	defer rows.Close()
	out := []ActionRun{}
	for rows.Next() {
		var r ActionRun
		var ended, code sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Path, &r.Action, &r.Cmd, &r.Mode, &r.DeviceID, &r.PID, &r.PIDStarted,
			&r.SessionID, &r.Status, &r.StartedAt, &ended, &code); err != nil {
			return nil, err
		}
		r.EndedAt = ended.Int64
		if code.Valid {
			c := int(code.Int64)
			r.ExitCode = &c
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
