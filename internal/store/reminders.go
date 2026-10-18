package store

import (
	"database/sql"
	"fmt"

	"github.com/google/uuid"

	"github.com/repogoapp/host/internal/errkind"
)

// ErrNoReminder is a reminder id this host does not hold.
var ErrNoReminder = errkind.New(errkind.NotFound, "store: no such reminder")

// Reminder is a one-shot alert to the phones at DueAt, milliseconds.
type Reminder struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Milliseconds.
	DueAt int64 `json:"due_at"`
	// The chat a tap on the alert opens; empty for none.
	ChatID string `json:"chat_id"`
	// The project; empty for none.
	Path string `json:"path"`
	// "user", or the chat whose agent set it.
	SetBy string `json:"set_by"`
	// Milliseconds.
	CreatedAt int64 `json:"created_at"`
}

// Reminders is every pending reminder, soonest first.
func (s *Store) Reminders() ([]Reminder, error) {
	rows, err := s.db.Query(`SELECT id, text, due_at, chat_id, path, set_by, created_at
		FROM state.reminders ORDER BY due_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: read reminders: %w", err)
	}
	defer rows.Close()
	out := []Reminder{}
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddReminder saves r with a new id, which it returns in the row.
func (s *Store) AddReminder(r Reminder) (Reminder, error) {
	r.ID = uuid.NewString()
	_, err := s.db.Exec(`INSERT INTO state.reminders (id, text, due_at, chat_id, path, set_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, r.ID, r.Text, r.DueAt, r.ChatID, r.Path, r.SetBy, r.CreatedAt)
	if err != nil {
		return Reminder{}, fmt.Errorf("store: add reminder: %w", err)
	}
	return r, nil
}

// DeleteReminder removes a pending reminder. Firing one deletes it too, so
// a delete returning true is the one claim on it: no reminder fires twice.
func (s *Store) DeleteReminder(id string) error {
	result, err := s.db.Exec(`DELETE FROM state.reminders WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete reminder: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNoReminder
	}
	return nil
}

func scanReminder(rows *sql.Rows) (Reminder, error) {
	var r Reminder
	err := rows.Scan(&r.ID, &r.Text, &r.DueAt, &r.ChatID, &r.Path, &r.SetBy, &r.CreatedAt)
	return r, err
}
