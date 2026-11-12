//go:build unix

package sqlitedb

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A database SQLite creates under a permissive umask is closed to other
// accounts once opened, with its -wal and -shm.
func TestADatabaseIsOnlyItsOwners(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	path := filepath.Join(t.TempDir(), "a.db")
	db := Open(path)
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t (x)"); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s is %o, want 600", filepath.Base(file), got)
		}
	}
}
