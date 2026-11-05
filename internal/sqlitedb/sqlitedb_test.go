package sqlitedb

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func pragma(t *testing.T, conn *sql.Conn, name string) string {
	t.Helper()
	var v string
	if err := conn.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

func TestEveryConnectionGetsThePragmas(t *testing.T) {
	db := Open(filepath.Join(t.TempDir(), "a.db"), "PRAGMA synchronous=FULL")
	defer db.Close()
	ctx := context.Background()

	// Two live connections at once, so the second is a new one.
	for i := range 2 {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		want := map[string]string{
			"journal_mode":         "wal",
			"checkpoint_fullfsync": "1",
			"fullfsync":            "0",
			"synchronous":          "2",
		}
		for name, v := range want {
			if got := pragma(t, conn, name); got != v {
				t.Errorf("connection %d: %s = %s, want %s", i, name, got, v)
			}
		}
	}
}

func TestABadPragmaFailsTheConnection(t *testing.T) {
	db := Open(filepath.Join(t.TempDir(), "a.db"), "PRAGMA nonsense(")
	defer db.Close()
	if err := db.Ping(); err == nil {
		t.Fatal("ping succeeded with a malformed pragma")
	}
}

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
