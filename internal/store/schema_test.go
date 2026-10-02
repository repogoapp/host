package store

import (
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/sqlitedb"
)

// A cache written by another schema is thrown away and rebuilt: keeping the
// old table would fail every sync against it, and the chat list would stop.
func TestOpenReplacesACacheFromAnotherSchema(t *testing.T) {
	dir := t.TempDir()
	old := sqlitedb.Open(filepath.Join(dir, CacheFile))
	for _, stmt := range []string{
		`CREATE TABLE sessions (sid INTEGER PRIMARY KEY, agent TEXT, session_id TEXT)`,
		`INSERT INTO sessions (agent, session_id) VALUES ('claude', 'stale')`,
		`PRAGMA user_version = 7`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open over an old schema: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	write(t, db, "fresh", 1000, 2)
	page, err := db.Chats(ChatQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Chats) != 1 || page.Chats[0].ID != "claude:fresh" {
		t.Fatalf("chats = %+v, want only the one synced after the rebuild", page.Chats)
	}
}

// The same schema keeps its rows across a restart.
func TestOpenKeepsACacheFromThisSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, db, "kept", 1000, 1)
	db.Close()

	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Info("claude:kept"); err != nil {
		t.Fatalf("row lost on reopen: %v", err)
	}
}
