package clitool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An unchanged binary answers from the cache; a replaced one (as an install or
// update leaves it) is asked again.
func TestVersionCachedWhileBinaryUnchanged(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	bin := filepath.Join(dir, "tool")
	write := func(version string) {
		script := "#!/bin/sh\necho run >> " + runs + "\necho " + version + "\n"
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		data, _ := os.ReadFile(runs)
		return strings.Count(string(data), "run")
	}
	write("1.2.3")
	in := NewInventory(nil, nil)
	if v := in.version(t.Context(), bin); v != "1.2.3" {
		t.Fatalf("version = %q", v)
	}
	if v := in.version(t.Context(), bin); v != "1.2.3" || count() != 1 {
		t.Fatalf("second read: %q after %d runs", v, count())
	}
	write("1.3.0")
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(bin, later, later); err != nil {
		t.Fatal(err)
	}
	if v := in.version(t.Context(), bin); v != "1.3.0" || count() != 2 {
		t.Fatalf("after update: %q after %d runs", v, count())
	}
}
