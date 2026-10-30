package github

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeList records its arguments and prints what `gh api /user/repos` prints:
// one of the account's repos and one it collaborates on.
const fakeList = `#!/bin/sh
echo "$@" > "$FAKE_GH/args"
cat <<'JSON'
[
  {"full_name":"octocat/old","name":"old","owner":{"login":"octocat"},"private":false,"pushed_at":"2025-01-01T00:00:00Z"},
  {"full_name":"hubot/new","name":"new","owner":{"login":"hubot"},"private":true,"pushed_at":"2026-09-01T00:00:00Z","description":null}
]
JSON
`

// The list is newest push first, marks what is already cloned, and includes
// repos the account collaborates on or reaches through an organization, which
// `gh repo list` leaves out.
func TestReposNewestFirstWithCollaborators(t *testing.T) {
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
	if len(repos) != 2 || repos[0].NameWithOwner != "hubot/new" || repos[1].Name != "old" {
		t.Fatalf("want new then old, got %+v", repos)
	}
	if repos[0].Path != "" || repos[1].Path == "" {
		t.Fatalf("only old is cloned: %+v", repos)
	}

	args, err := os.ReadFile(filepath.Join(gh, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "affiliation=owner,collaborator,organization_member") {
		t.Fatalf("repo list leaves out collaborator and organization repos: %s", args)
	}
}
