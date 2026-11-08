package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// PluginKey is an environment plugin's key and when it was made, ms; empty
// when the plugin has none.
func (s *Store) PluginKey(plugin string) (key string, createdAt int64, err error) {
	err = s.db.QueryRow(`SELECT key, created_at FROM state.plugin_keys WHERE plugin = ?`, plugin).Scan(&key, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("store: read plugin key: %w", err)
	}
	return key, createdAt, nil
}

// SetPluginKey saves plugin's key, made at createdAt (ms), replacing the one it had.
func (s *Store) SetPluginKey(plugin, key string, createdAt int64) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO state.plugin_keys (plugin, key, created_at) VALUES (?, ?, ?)`,
		plugin, key, createdAt)
	if err != nil {
		return fmt.Errorf("store: save plugin key: %w", err)
	}
	return nil
}

// DeletePluginKey removes plugin's key; a plugin with none is already done.
func (s *Store) DeletePluginKey(plugin string) error {
	if _, err := s.db.Exec(`DELETE FROM state.plugin_keys WHERE plugin = ?`, plugin); err != nil {
		return fmt.Errorf("store: delete plugin key: %w", err)
	}
	return nil
}
