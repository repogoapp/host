package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// HostName is the name a device gave this host; "" when none has.
func (s *Store) HostName() (string, error) {
	var name string
	err := s.db.QueryRow(`SELECT name FROM state.host_name WHERE id = 1`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read host name: %w", err)
	}
	return name, nil
}

// SetHostName saves name, the last to arrive winning; "" clears it.
func (s *Store) SetHostName(name string) error {
	var err error
	if name == "" {
		_, err = s.db.Exec(`DELETE FROM state.host_name WHERE id = 1`)
	} else {
		_, err = s.db.Exec(`INSERT INTO state.host_name (id, name) VALUES (1, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name`, name)
	}
	if err != nil {
		return fmt.Errorf("store: save host name: %w", err)
	}
	return nil
}
