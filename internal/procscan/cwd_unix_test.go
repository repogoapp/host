//go:build !windows

package procscan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCwdsResolvesThisProcess(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil && runtime.GOOS != "linux" {
		t.Skip("lsof is not installed")
	}
	want, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	got := Cwds(context.Background(), []int{pid})[pid]
	if resolve(t, got) != resolve(t, want) {
		t.Errorf("cwd = %q, want %q", got, want)
	}
}

func resolve(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %q: %v", path, err)
	}
	return resolved
}
