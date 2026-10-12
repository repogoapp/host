package store

import "database/sql"

// assignVoiceNumbers numbers every chat without one, oldest first, bumping each
// row's rev so devices fetch it. The voice agent names a chat by its number
// ("A-1027" with the phone's environment letter), so none is reused.
func (s *Store) assignVoiceNumbers(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT s.agent, s.session_id FROM sessions s
		WHERE NOT EXISTS (SELECT 1 FROM state.voice_numbers v WHERE v.agent = s.agent AND v.session_id = s.session_id)
		ORDER BY s.created_at, s.updated_at, s.sid`)
	if err != nil {
		return err
	}
	type chat struct{ agent, sessionID string }
	var unnumbered []chat
	for rows.Next() {
		var c chat
		if err := rows.Scan(&c.agent, &c.sessionID); err != nil {
			rows.Close()
			return err
		}
		unnumbered = append(unnumbered, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, c := range unnumbered {
		if _, err := tx.Exec(`INSERT INTO state.voice_numbers (agent, session_id) VALUES (?, ?)`, c.agent, c.sessionID); err != nil {
			return err
		}
		if err := s.bumpRev(tx, c.agent, c.sessionID); err != nil {
			return err
		}
	}
	return nil
}

// numberChats numbers the chats no import has, such as every chat when
// state.db is new. It runs at open, before anything listens.
func (s *Store) numberChats() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.assignVoiceNumbers(tx); err != nil {
		return err
	}
	return tx.Commit()
}
