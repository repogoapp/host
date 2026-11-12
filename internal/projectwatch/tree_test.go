//go:build darwin || linux

package projectwatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/testwait"
)

// Where the system has a tree watcher, a file written under a watched folder
// reaches the room by name, with no timer in between.
func TestAWrittenFileReachesTheRoomByName(t *testing.T) {
	dir := t.TempDir()
	m := newManager(&fakeGit{})
	m.linger = time.Millisecond
	pub := m.pub.(*fakePub)
	if err := m.Watch("a", []string{dir}, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop("a") })

	file := filepath.Join(dir, "src", "app.ts")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	// Written until heard: the watcher starts on its own goroutine.
	testwait.For(t, "fs.changed naming src/app.ts", func() bool {
		if err := os.WriteFile(file, []byte(time.Now().String()), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, payload := range pub.filePushes() {
			var got FilesChanged
			if err := json.Unmarshal(payload, &got); err != nil {
				t.Fatal(err)
			}
			for _, c := range got.Changes {
				if got.Path == dir && c.File == "src/app.ts" {
					return true
				}
			}
		}
		return false
	})
}
