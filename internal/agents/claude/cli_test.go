package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// An old npm global earlier on PATH never answers `list_models`, so the native
// install wins whenever there is one.
func TestClaudeExecutablePrefersTheNativeInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := executable(); got != "claude" {
		t.Fatalf("no native install: got %q, want PATH lookup", got)
	}

	native := filepath.Join(home, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(native), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := executable(); got != native {
		t.Fatalf("got %q, want %q", got, native)
	}
}
