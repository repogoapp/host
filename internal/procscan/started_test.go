//go:build darwin || linux

package procscan

import (
	"os"
	"testing"
)

func TestStartedNamesThisProcess(t *testing.T) {
	first, ok := Started(os.Getpid())
	if !ok || first == 0 {
		t.Fatalf("Started(self) = %d, %v", first, ok)
	}
	if again, _ := Started(os.Getpid()); again != first {
		t.Errorf("Started moved: %d then %d", first, again)
	}
	if _, ok := Started(-1); ok {
		t.Error("Started(-1) found a process")
	}
}
