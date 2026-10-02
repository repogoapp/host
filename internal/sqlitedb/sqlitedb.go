// Package sqlitedb opens the host's SQLite databases with their pragmas set on
// every connection the pool opens, not only the first.
package sqlitedb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"

	"github.com/mattn/go-sqlite3"
)

type connector struct {
	driver *sqlite3.SQLiteDriver
	path   string
}

func (c connector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.path) }
func (c connector) Driver() driver.Driver                        { return c.driver }

// Open returns a WAL database at path. Each new connection runs the base
// pragmas, then pragmas in order.
func Open(path string, pragmas ...string) *sql.DB {
	all := append([]string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA checkpoint_fullfsync=ON",
	}, pragmas...)
	d := &sqlite3.SQLiteDriver{ConnectHook: func(conn *sqlite3.SQLiteConn) error {
		for _, pragma := range all {
			if _, err := conn.Exec(pragma, nil); err != nil {
				return fmt.Errorf("%s: %w", pragma, err)
			}
		}
		return nil
	}}
	return sql.OpenDB(connector{d, path})
}
