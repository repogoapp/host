package host_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/repogoapp/host/internal/host"
	"github.com/repogoapp/host/internal/testhost"
)

// Starting the host closes a state folder another account could read: the
// folder 0700, a database left 0644 from before 0600.
func TestTheHostClosesItsStateFolderAtStartup(t *testing.T) {
	var state string
	testhost.New(t, func(c *host.Config) {
		state = c.State
		if err := os.MkdirAll(state, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(state, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "old.db"), []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	for name, want := range map[string]os.FileMode{".": 0o700, "old.db": 0o600} {
		info, err := os.Stat(filepath.Join(state, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s is %o, want %o", name, got, want)
		}
	}
}
