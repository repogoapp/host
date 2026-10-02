package agents_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// writeFile writes content to path, making its directory, and returns path.
func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeLines writes one line per argument.
func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
}

// jsonl writes one JSON object per line.
func jsonl(t *testing.T, path string, lines ...any) {
	t.Helper()
	var out []string
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(raw))
	}
	writeLines(t, path, out...)
}
