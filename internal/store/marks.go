package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ChatMarks is what RepoGo knows about a chat that its transcript doesn't, one
// field per column of state.chat_marks. Nil is not set.
type ChatMarks struct {
	ResolvedAt *int64
	PinnedAt   *int64
	ParentID   *ChatID
}

func (m ChatMarks) empty() bool { return m.ResolvedAt == nil && m.PinnedAt == nil && m.ParentID == nil }

// updateChatMarks is the one writer of state.chat_marks outside the import.
// change edits the marks it is handed and reports whether it changed any; the
// row is written whole, deleted when nothing is set, then the chat fans out.
func (s *Store) updateChatMarks(id ChatID, change func(*ChatMarks) bool) (bool, error) {
	agent, sessionID, err := id.parts()
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var marks ChatMarks
	err = tx.QueryRow(`SELECT resolved_at, pinned_at, parent_id FROM state.chat_marks WHERE agent = ? AND session_id = ?`,
		agent, sessionID).Scan(&marks.ResolvedAt, &marks.PinnedAt, &marks.ParentID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("store: read chat marks: %w", err)
	}
	if !change(&marks) {
		return false, nil
	}

	if marks.empty() {
		_, err = tx.Exec(`DELETE FROM state.chat_marks WHERE agent = ? AND session_id = ?`, agent, sessionID)
	} else {
		_, err = tx.Exec(`INSERT INTO state.chat_marks (agent, session_id, resolved_at, pinned_at, parent_id) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (agent, session_id) DO UPDATE SET resolved_at = excluded.resolved_at, pinned_at = excluded.pinned_at,
			parent_id = excluded.parent_id`,
			agent, sessionID, marks.ResolvedAt, marks.PinnedAt, marks.ParentID)
	}
	if err != nil {
		return false, fmt.Errorf("store: write chat marks: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	// State commits first and stands alone; the rev bump is recomputable.
	return true, s.touch(id)
}

// SetResolved marks a chat's work done, or clears the mark. Resolving does not
// touch activity_at: the chat leaves the inbox but keeps its place in time, so
// unresolving puts it back where it was rather than at the top.
func (s *Store) SetResolved(id ChatID, resolved bool, at time.Time) error {
	_, err := s.updateChatMarks(id, func(m *ChatMarks) bool {
		if resolved {
			m.ResolvedAt = ptr(at.UnixMilli())
		} else {
			m.ResolvedAt = nil
		}
		return true
	})
	return err
}

// SetChatPinned pins a chat to the top of every device's list, or unpins it.
// Unlike a resolve, activity in the chat leaves a pin alone.
func (s *Store) SetChatPinned(id ChatID, pinned bool, at time.Time) error {
	_, err := s.updateChatMarks(id, func(m *ChatMarks) bool {
		if pinned {
			m.PinnedAt = ptr(at.UnixMilli())
		} else {
			m.PinnedAt = nil
		}
		return true
	})
	return err
}

// SetChatParent records the chat whose agent started this one. A chat with a
// parent stays out of the drawer and is listed under its parent instead.
func (s *Store) SetChatParent(id, parent ChatID) error {
	if _, _, err := parent.parts(); err != nil {
		return err
	}
	_, err := s.updateChatMarks(id, func(m *ChatMarks) bool {
		if m.ParentID != nil && *m.ParentID == parent {
			return false
		}
		m.ParentID = &parent
		return true
	})
	return err
}

// UnresolveIfActive clears a resolve older than a prompt at `at`: nobody types
// "unresolve" before continuing a conversation. Activity older than the mark
// is a replay and leaves it. Reports whether a mark was cleared.
func (s *Store) UnresolveIfActive(id ChatID, at time.Time) (bool, error) {
	return s.updateChatMarks(id, func(m *ChatMarks) bool {
		if m.ResolvedAt == nil || *m.ResolvedAt >= at.UnixMilli() {
			return false
		}
		m.ResolvedAt = nil
		return true
	})
}

// SetTitle shows a rename at once. The provider's file holds the title of
// record, and the next sync reads the same one back from it.
func (s *Store) SetTitle(id ChatID, title string) error {
	agent, sessionID, err := id.parts()
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE sessions SET title = ? WHERE agent = ? AND session_id = ?`,
		collapseSpace(title), agent, sessionID); err != nil {
		return fmt.Errorf("store: set title: %w", err)
	}
	return s.touch(id)
}

// execer is a *sql.DB or a *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// bumpRev advances a chat's revision for a change that lives outside its row.
func (s *Store) bumpRev(db execer, agent, sessionID string) error {
	_, err := db.Exec(`UPDATE sessions SET rev = ? WHERE agent = ? AND session_id = ?`,
		s.rev.Add(1), agent, sessionID)
	return err
}

// touch is bumpRev outside a transaction, so the listener hears it at once.
func (s *Store) touch(id ChatID) error {
	agent, sessionID, _ := id.split()
	if err := s.bumpRev(s.db, agent, sessionID); err != nil {
		return err
	}
	s.notify(Change{Changed: []ChatID{id}})
	return nil
}

func ptr[T any](v T) *T { return &v }
