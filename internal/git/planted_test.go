package git_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A cloned repo can't ship a .git, but it can ship a folder laid out like one.
// Git would take that folder for a repository and run the commands its config
// names, so no read may treat it as one.
func TestAPlantedGitFolderRunsNothing(t *testing.T) {
	svc, dir := repo(t)
	marker := filepath.Join(dir, "ran")
	planted := filepath.Join(dir, "docs", "assets")
	for _, sub := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(planted, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, planted, "HEAD", "ref: refs/heads/main\n")
	write(t, planted, "config", "[core]\n\trepositoryformatversion = 0\n\tbare = false\n\tworktree = "+dir+"\n\tfsmonitor = \"touch '"+marker+"'; false\"\n")

	ctx := context.Background()
	if _, err := svc.Status(ctx, []string{planted}); err != nil {
		t.Fatalf("status: %v", err)
	}
	_, _ = svc.Changes(ctx, planted)
	_, _ = svc.Diff(ctx, planted)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git ran the planted folder's core.fsmonitor command")
	}
}
