package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// IdempotencyKey is a finished request's hash and answer, ok false when
// none is kept under (scope, method, key).
func (s *Store) IdempotencyKey(scope, method, key string) (hash, result []byte, ok bool, err error) {
	err = s.db.QueryRow(`SELECT request_hash, result FROM state.idempotency_keys
		WHERE scope = ? AND method = ? AND key = ?`, scope, method, key).Scan(&hash, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("store: read idempotency key: %w", err)
	}
	return hash, result, true, nil
}

// SaveIdempotencyKey keeps a finished request's answer, at createdAt (ms).
func (s *Store) SaveIdempotencyKey(scope, method, key string, hash, result []byte, createdAt int64) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO state.idempotency_keys
		(scope, method, key, request_hash, result, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		scope, method, key, hash, result, createdAt)
	if err != nil {
		return fmt.Errorf("store: save idempotency key: %w", err)
	}
	return nil
}

// DeleteIdempotencyKey drops a kept answer, so the next request with its
// key runs anew.
func (s *Store) DeleteIdempotencyKey(scope, method, key string) error {
	_, err := s.db.Exec(`DELETE FROM state.idempotency_keys WHERE scope = ? AND method = ? AND key = ?`,
		scope, method, key)
	if err != nil {
		return fmt.Errorf("store: delete idempotency key: %w", err)
	}
	return nil
}

// SetTurnChat records the chat a turn opened, at createdAt (ms).
func (s *Store) SetTurnChat(turnID string, chat ChatID, createdAt int64) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO state.turn_chats (turn_id, chat_id, created_at) VALUES (?, ?, ?)`,
		turnID, string(chat), createdAt)
	if err != nil {
		return fmt.Errorf("store: save turn chat: %w", err)
	}
	return nil
}

// TurnChat is the chat a turn opened; empty when none is recorded.
func (s *Store) TurnChat(turnID string) (ChatID, error) {
	var chat string
	err := s.db.QueryRow(`SELECT chat_id FROM state.turn_chats WHERE turn_id = ?`, turnID).Scan(&chat)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read turn chat: %w", err)
	}
	return ChatID(chat), nil
}

// PruneIdempotency drops kept answers and turn chats older than before (ms).
func (s *Store) PruneIdempotency(before int64) error {
	for _, table := range []string{"idempotency_keys", "turn_chats"} {
		if _, err := s.db.Exec(`DELETE FROM state.`+table+` WHERE created_at < ?`, before); err != nil {
			return fmt.Errorf("store: prune %s: %w", table, err)
		}
	}
	return nil
}
