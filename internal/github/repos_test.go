package github

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeList records its arguments and prints what `gh repo list --json` prints.
const fakeList = `#!/bin/sh
echo "$@" > "$FAKE_GH/args"
cat <<'JSON'
[
  {"nameWithOwner":"octocat/old","name":"old","owner":{"login":"octocat"},"isPrivate":false,"pushedAt":"2025-01-01T00:00:00Z"},
  {"nameWithOwner":"octocat/new","name":"new","owner":{"login":"octocat"},"isPrivate":true,"pushedAt":"2026-09-01T00:00:00Z"}
]
JSON
`

// The list is newest push first, marks what is already cloned, and does not
// ask GitHub for each repository's default branch: that field alone tripled
// the call, and a checkout reads its own from origin/HEAD.
func TestReposNewestFirstWithoutDefaultBranch(t *testing.T) {
	gh := t.TempDir()
	if err := os.WriteFile(filepath.Join(gh, "gh"), []byte(fakeList), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", gh+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH", gh)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}

	repos, err := newService(t, dir).Repos(context.Background(), "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].Name != "new" || repos[1].Name != "old" {
		t.Fatalf("want new then old, got %+v", repos)
	}
	if repos[0].Path != "" || repos[1].Path == "" {
		t.Fatalf("only old is cloned: %+v", repos)
	}

	args, err := os.ReadFile(filepath.Join(gh, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "defaultBranchRef") {
		t.Fatalf("repo list asks for the default branch: %s", args)
	}
}
