package store

import "testing"

func TestOpenSetsTheSyncPragmas(t *testing.T) {
	db := newStore(t)
	for name, want := range map[string]string{
		"journal_mode":         "wal",
		"synchronous":          "1",
		"checkpoint_fullfsync": "1",
		"fullfsync":            "0",
	} {
		var got string
		if err := db.db.QueryRow("PRAGMA " + name).Scan(&got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
}
