package plugins

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestTargetsAtHomeListTheEnvironmentOnce reads the home folder, whose
// .repogo/plugins is the environment's own: one environment target, no project one.
func TestTargetsAtHomeListTheEnvironmentOnce(t *testing.T) {
	home := t.TempDir()
	global := filepath.Join(home, filepath.FromSlash(Folder))
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: Config{Runtime: "run.ts", Global: global}}
	got, err := s.targets(home)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Target{{Dir: global, Root: home}}; !slices.Equal(got, want) {
		t.Errorf("targets(home) = %v, want %v", got, want)
	}
}
