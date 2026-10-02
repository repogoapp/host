package sqlitedb

import (
	"context"
	"database/sql"
	"path/filepath"
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
