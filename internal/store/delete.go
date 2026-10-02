package store

import (
	"database/sql"
	"fmt"
)

// Delete drops one chat: its events, its search row, its row, and the user's
// marks and handle.
// The provider file is already gone by the time this is called.
func (s *Store) Delete(id ChatID) error {
	kind, sessionID, err := id.parts()
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM events WHERE sid IN (SELECT sid FROM sessions WHERE agent = ? AND session_id = ?)`, kind, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(deleteSearchRow, kind, sessionID); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM sessions WHERE agent = ? AND session_id = ?`, kind, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: chat %q", ErrNotFound, id)
	}
	if err := dropChatState(tx, kind, sessionID); err != nil {
		return err
	}
	if err := s.keepRevFloor(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify(Change{Removed: []ChatID{id}})
	return nil
}

// Prune deletes every synced session whose key is not in keep, a complete
// listing, and reports how many went. Rows SetStatus inserted before their
// transcript exists were never synced, so they stay.
func (s *Store) Prune(keep map[string]bool) (int, error) {
	synced, err := selectChats(s.db, `SELECT agent, session_id FROM sessions WHERE synced_at > 0`)
	if err != nil {
		return 0, err
	}
	var stale []ChatID
	for _, id := range synced {
		if kind, sessionID, _ := id.split(); !keep[key(kind, sessionID)] {
			stale = append(stale, id)
		}
	}
	if len(stale) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, id := range stale {
		kind, sessionID, _ := id.split()
		if _, err := tx.Exec(`DELETE FROM events WHERE sid IN (SELECT sid FROM sessions WHERE agent = ? AND session_id = ?)`, kind, sessionID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(deleteSearchRow, kind, sessionID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM sessions WHERE agent = ? AND session_id = ?`, kind, sessionID); err != nil {
			return 0, err
		}
		if err := dropChatState(tx, kind, sessionID); err != nil {
			return 0, err
		}
	}
	if err := s.keepRevFloor(tx); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.notify(Change{Removed: stale})
	return len(stale), nil
}

// querier is a *sql.DB or a *sql.Tx.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// selectChats runs a query whose columns are agent and session_id.
func selectChats(db querier, query string, args ...any) ([]ChatID, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []ChatID
	for rows.Next() {
		var kind, sessionID string
		if err := rows.Scan(&kind, &sessionID); err != nil {
			return nil, err
		}
		ids = append(ids, chatID(kind, sessionID))
	}
	return ids, rows.Err()
}

// dropChatState removes one deleted chat's resolve and voice handle. Only
// that chat's: a chat missing from the cache may be not yet re-imported, and
// its state is still the user's.
func dropChatState(tx *sql.Tx, kind, sessionID string) error {
	if _, err := tx.Exec(`DELETE FROM state.chat_marks WHERE agent = ? AND session_id = ?`, kind, sessionID); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM state.voice_handles WHERE agent = ? AND session_id = ?`, kind, sessionID)
	return err
}
