package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/errkind"
)

// ErrNoSchedule is a schedule id this host does not hold.
var ErrNoSchedule = errkind.New(errkind.NotFound, "store: no such schedule")

// Schedule is what the user sets on a schedule: a prompt this host starts as
// a new chat in a project at a set time. schedules.save takes it whole.
type Schedule struct {
	ID     string           `json:"id"`
	Title  string           `json:"title"`
	Prompt string           `json:"prompt"`
	Path   string           `json:"path"`
	Agent  string           `json:"agent"`
	Config agent.TurnConfig `json:"config"`
	// hourly | daily | weekly; Hour is unused hourly, Weekday unless weekly.
	Frequency string `json:"frequency"`
	Hour      int    `json:"hour"`
	Minute    int    `json:"minute"`
	// 1..7, Sunday = 1.
	Weekday  int    `json:"weekday"`
	Timezone string `json:"timezone"`
	Enabled  bool   `json:"enabled"`
}

// SavedSchedule is a schedule with the two times its ticker reads.
type SavedSchedule struct {
	Schedule
	SavedAt   time.Time
	LastDueAt time.Time // zero before its first due time is taken
}

// Schedules is every schedule on this host; the phone orders them by next run.
func (s *Store) Schedules() ([]SavedSchedule, error) {
	rows, err := s.db.Query(`SELECT id, title, prompt, path, agent, config, frequency, hour, minute,
		weekday, timezone, enabled, saved_at, last_due_at FROM state.schedules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: read schedules: %w", err)
	}
	defer rows.Close()
	out := []SavedSchedule{}
	for rows.Next() {
		saved, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, saved)
	}
	return out, rows.Err()
}

// SaveSchedule is the one writer of a schedule's definition: it makes one
// when sc.ID is empty, minting its id, or replaces the one with that id.
// Saving moves saved_at to at, so no time at or before it runs.
func (s *Store) SaveSchedule(sc Schedule, at time.Time) (Schedule, error) {
	config, err := json.Marshal(sc.Config)
	if err != nil {
		return Schedule{}, fmt.Errorf("store: save schedule: %w", err)
	}
	if sc.ID == "" {
		sc.ID = uuid.NewString()
		_, err = s.db.Exec(`INSERT INTO state.schedules (id, title, prompt, path, agent, config, frequency,
			hour, minute, weekday, timezone, enabled, saved_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sc.ID, sc.Title, sc.Prompt, sc.Path, sc.Agent, config, sc.Frequency,
			sc.Hour, sc.Minute, sc.Weekday, sc.Timezone, sc.Enabled, at.UnixMilli())
		if err != nil {
			return Schedule{}, fmt.Errorf("store: save schedule: %w", err)
		}
		return sc, nil
	}
	result, err := s.db.Exec(`UPDATE state.schedules SET title = ?, prompt = ?, path = ?, agent = ?, config = ?,
		frequency = ?, hour = ?, minute = ?, weekday = ?, timezone = ?, enabled = ?, saved_at = ? WHERE id = ?`,
		sc.Title, sc.Prompt, sc.Path, sc.Agent, config, sc.Frequency,
		sc.Hour, sc.Minute, sc.Weekday, sc.Timezone, sc.Enabled, at.UnixMilli(), sc.ID)
	if err != nil {
		return Schedule{}, fmt.Errorf("store: save schedule: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Schedule{}, ErrNoSchedule
	}
	return sc, nil
}

// DeleteSchedule removes a schedule. The chats its runs started stay.
func (s *Store) DeleteSchedule(id string) error {
	result, err := s.db.Exec(`DELETE FROM state.schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete schedule: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNoSchedule
	}
	return nil
}

// ClaimScheduleRun takes due for the schedule as it was saved at savedAt. It
// is false when the time is taken already, or the schedule was disabled,
// edited or deleted since the caller read it, so a stale prompt never runs.
func (s *Store) ClaimScheduleRun(id string, savedAt, due time.Time) (bool, error) {
	result, err := s.db.Exec(`UPDATE state.schedules SET last_due_at = ?
		WHERE id = ? AND enabled = 1 AND saved_at = ? AND (last_due_at IS NULL OR last_due_at < ?)`,
		due.UnixMilli(), id, savedAt.UnixMilli(), due.UnixMilli())
	if err != nil {
		return false, fmt.Errorf("store: claim schedule run: %w", err)
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func scanSchedule(rows *sql.Rows) (SavedSchedule, error) {
	var out SavedSchedule
	var config []byte
	var savedAt int64
	var lastDueAt sql.NullInt64
	err := rows.Scan(&out.ID, &out.Title, &out.Prompt, &out.Path, &out.Agent, &config, &out.Frequency,
		&out.Hour, &out.Minute, &out.Weekday, &out.Timezone, &out.Enabled, &savedAt, &lastDueAt)
	if err != nil {
		return SavedSchedule{}, err
	}
	if err := json.Unmarshal(config, &out.Config); err != nil {
		return SavedSchedule{}, fmt.Errorf("store: schedule %s: %w", out.ID, err)
	}
	out.SavedAt = time.UnixMilli(savedAt)
	if lastDueAt.Valid {
		out.LastDueAt = time.UnixMilli(lastDueAt.Int64)
	}
	return out, nil
}
