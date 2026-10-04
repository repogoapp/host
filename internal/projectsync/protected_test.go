package projectsync

import (
	"log/slog"
	"testing"
	"time"

	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/testwait"
)

func TestProtectedFolders(t *testing.T) {
	p := newProtected("darwin", "/Users/me")
	for path, want := range map[string]string{
		"/Users/me/Desktop":                  "/Users/me/Desktop",
		"/Users/me/Desktop/game":             "/Users/me/Desktop",
		"/Users/me/Documents/work/app":       "/Users/me/Documents",
		"/Users/me/Library/Mobile Documents": "/Users/me/Library/Mobile Documents",
		"/Volumes/Share/repo":                "/Volumes/Share",
		"/Users/me/RepoGo/app":               "",
		"/Users/me/DesktopApps":              "",
		"/tmp/p":                             "",
	} {
		if got := p.rootOf(path); got != want {
			t.Errorf("rootOf(%q) = %q, want %q", path, got, want)
		}
	}
	if got := newProtected("linux", "/home/me").rootOf("/home/me/Desktop/app"); got != "" {
		t.Errorf("linux protects %q", got)
	}
}

// A folder whose first read waits on a prompt is skipped, told once, and read
// again once that read returns, as when the user allows it.
func TestAFolderWaitingOnAPromptIsSkippedUntilItAnswers(t *testing.T) {
	p := newProtected("darwin", "/Users/me")
	p.wait = 10 * time.Millisecond
	answer := make(chan struct{})
	p.open = func(string) error {
		<-answer
		return nil
	}
	var stuck []string
	p.stuck = func(root string) { stuck = append(stuck, root) }

	if p.readable("/Users/me/Desktop/game") || p.readable("/Users/me/Desktop") {
		t.Fatal("read inside a folder waiting on a prompt")
	}
	if !p.readable("/Users/me/RepoGo/app") {
		t.Fatal("an unprotected folder is not readable")
	}
	if len(stuck) != 1 || stuck[0] != "/Users/me/Desktop" {
		t.Fatalf("stuck %v", stuck)
	}
	close(answer)
	testwait.For(t, "the folder after the prompt answered", func() bool { return p.readable("/Users/me/Desktop/game") })
}

type storedRows struct {
	recorded
	stored []store.Project
}

func (s *storedRows) ProjectsAt([]string) ([]store.Project, error) { return s.stored, nil }

type countedIcons struct{ asked []string }

func (c *countedIcons) IconHash(path string) string {
	c.asked = append(c.asked, path)
	return ""
}

// A pass that cannot read inside a folder keeps what an earlier pass found
// there rather than looking, or erasing it.
func TestAnUnreadableFolderKeepsItsStoredRow(t *testing.T) {
	db := &storedRows{stored: []store.Project{{
		Path: "/Users/me/Desktop/game", Kind: store.ProjectClone, IconHash: "abc", RepoOwner: "me", RepoName: "game",
	}}}
	icons := &countedIcons{}
	s := New(listed{"/Users/me/Desktop/game", "/tmp/p"}, db, icons, "", "/Users/me", ignore, slog.New(slog.DiscardHandler))
	s.access = newProtected("darwin", "/Users/me")
	s.access.wait = time.Millisecond
	block := make(chan struct{})
	defer close(block)
	s.access.open = func(string) error { <-block; return nil }

	s.Once(t.Context())

	if len(icons.asked) != 1 || icons.asked[0] != "/tmp/p" {
		t.Fatalf("asked icons for %v", icons.asked)
	}
	got := db.rows[0]
	if got.Path != "/Users/me/Desktop/game" || got.Kind != store.ProjectClone || got.IconHash != "abc" || got.RepoName != "game" {
		t.Fatalf("row %+v", got)
	}
}
