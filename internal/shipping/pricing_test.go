package shipping

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A stored table is valid by when it was fetched, even an empty one: a CI run
// with a fresh empty cache must not reach models.dev or GitHub.
func TestStoredEmptyRatesAreNotRefetched(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "rates.json")
	if err := os.WriteFile(path, []byte(`{"fetchedAtMs":`+strconv.FormatInt(now.UnixMilli(), 10)+`,"rates":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &pricing{path: path, fetch: func(context.Context) rates {
		t.Fatal("downloaded rates the cache already had")
		return nil
	}}
	if got := p.table(t.Context(), now); len(got) != 0 {
		t.Fatalf("rates = %v", got)
	}
}

// A downloaded table is saved and read back by the next host.
func TestFetchedRatesRoundTrip(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "rates.json")
	want := rates{"m": {input: 1, output: 2, cacheRead: 3, cacheCreation: 4, fast: 2}}
	(&pricing{path: path, fetch: func(context.Context) rates { return want }}).table(t.Context(), now)

	p := &pricing{path: path, fetch: func(context.Context) rates {
		t.Fatal("refetched a saved table")
		return nil
	}}
	if got := p.table(t.Context(), now); got["m"] != want["m"] {
		t.Fatalf("read back %v, want %v", got, want)
	}
}
