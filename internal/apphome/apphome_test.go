package apphome

import (
	"os"
	"path/filepath"
	"testing"
)

// A secret whose mode was widened by hand is tightened on the next write.
func TestWriteFileTightensAWidenedSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte(`{"k":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

func TestReadJSONRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	var missing map[string]int
	if found, err := ReadJSON(path, &missing); found || err != nil || missing != nil {
		t.Fatalf("missing file: found=%v err=%v v=%v", found, err, missing)
	}
	if err := WriteJSON(path, map[string]int{"a": 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	if found, err := ReadJSON(path, &got); !found || err != nil || got["a"] != 1 {
		t.Fatalf("found=%v err=%v v=%v", found, err, got)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if found, err := ReadJSON(path, &got); !found || err == nil {
		t.Fatalf("corrupt file: found=%v err=%v", found, err)
	}
}

// REPOGO_HOME replaces the whole directory, so tests never touch ~/.repogo.
func TestPathHonoursTheOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvVar, dir)
	path, err := MkdirAll("hooks", "repogo-notify")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "hooks", "repogo-notify"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("parent not created: %v", err)
	}
}

// A readable state folder is closed: the folder 0700, databases and
// crons.json 0600; other files, and modes already closed, keep theirs.
func TestSecureClosesTheStateFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".repogo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]os.FileMode{
		"cache.db": 0o644, "cache.db-wal": 0o644, "cache.db-shm": 0o644,
		"usage.db.before-prompt": 0o644, "crons.json": 0o644,
		"device.json": 0o400, "notes.txt": 0o644,
	}
	for name, mode := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Secure(dir); err != nil {
		t.Fatal(err)
	}
	want := map[string]os.FileMode{
		".": 0o700, "cache.db": 0o600, "cache.db-wal": 0o600, "cache.db-shm": 0o600,
		"usage.db.before-prompt": 0o600, "crons.json": 0o600,
		// Already closed, kept; not private, left; folders inside are behind the 0700.
		"device.json": 0o400, "notes.txt": 0o644, "plugins": 0o755,
	}
	for name, mode := range want {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != mode {
			t.Errorf("%s is %o, want %o", name, got, mode)
		}
	}
}
