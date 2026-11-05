// Package sqlitedb opens the host's SQLite databases with their pragmas set on
// every connection the pool opens, not only the first.
package sqlitedb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"

	"github.com/mattn/go-sqlite3"
)

type connector struct {
	driver *sqlite3.SQLiteDriver
	path   string
}

func (c connector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.path) }
func (c connector) Driver() driver.Driver                        { return c.driver }

// owned makes the database and its -wal and -shm 0600 once they exist:
// SQLite creates them under the umask, readable by every account. A path
// that isn't a file (":memory:", a URI) is left alone.
func owned(path string) {
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(file); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 != 0 {
			_ = os.Chmod(file, info.Mode().Perm()&0o600)
		}
	}
}

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
		owned(path)
		return nil
	}}
	return sql.OpenDB(connector{d, path})
}
