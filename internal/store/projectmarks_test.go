package store

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestAPickedFolderIsARootBeforeAnyChat(t *testing.T) {
	db := newStore(t)
	// An hour ago, so the pick made now sorts above it.
	write(t, db, "old", time.Now().Add(-time.Hour).UnixMilli(), 1)
	now := time.Now()
	if err := db.Pick("/tmp/picked", now); err != nil {
		t.Fatal(err)
	}
	roots, err := db.Roots()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/tmp/picked", "/tmp/old"}; !slices.Equal(roots, want) {
		t.Fatalf("roots = %v, want %v", roots, want)
	}

	// Picking a folder a chat already ran in adds nothing twice.
	if err := db.Pick("/tmp/old", now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if roots, _ := db.Roots(); len(roots) != 2 || roots[0] != "/tmp/old" {
		t.Fatalf("roots after re-pick = %v, want /tmp/old first of two", roots)
	}
}

// A project is called by the user's name, else its repository spelled as the
// folder spells it, else its folder; a blank rename goes back.
func TestProjectNamesFollowOneRule(t *testing.T) {
	db := newStore(t)
	if _, err := db.SyncProjects([]Project{
		{Path: "/src/RepoGo", Kind: ProjectClone, RepoOwner: "alice", RepoName: "repogo", ActivityAt: 1},
		{Path: "/src/codev2", Kind: ProjectClone, RepoOwner: "alice", RepoName: "repogo", ActivityAt: 2},
		{Path: "/notes", Kind: ProjectFolder, ActivityAt: 3},
	}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/src/RepoGo": "RepoGo", "/src/codev2": "repogo", "/notes": "notes"} {
		if row, _ := db.Project(path); row.DisplayName != want {
			t.Errorf("%s is called %q, want %q", path, row.DisplayName, want)
		}
	}

	row, err := db.Rename("/src/codev2", "  RepoGo   v2 ")
	if err != nil || row.DisplayName != "RepoGo v2" {
		t.Fatalf("rename = %q, %v", row.DisplayName, err)
	}
	if name := db.ProjectName("/src/codev2"); name != "RepoGo v2" {
		t.Fatalf("ProjectName = %q", name)
	}
	if row, _ := db.Rename("/src/codev2", "  "); row.DisplayName != "repogo" {
		t.Fatalf("blank rename = %q, want the repository back", row.DisplayName)
	}
	if _, err := db.Rename("/not/listed", "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("renaming an unlisted folder = %v, want ErrInvalid", err)
	}
	if name := db.ProjectName("/not/listed"); name != "listed" {
		t.Fatalf("an unlisted folder's name = %q", name)
	}
}

// A pinned project is listed even past the cap, and unpinning clears it.
func TestAPinnedProjectIsAlwaysListed(t *testing.T) {
	db := newStore(t)
	rows := make([]Project, maxProjects+1)
	for i := range rows {
		rows[i] = Project{Path: fmt.Sprintf("/p/%03d", i), Kind: ProjectFolder, ActivityAt: int64(i + 1)}
	}
	if _, err := db.SyncProjects(rows); err != nil {
		t.Fatal(err)
	}
	// /p/000 is the oldest, past the newest maxProjects.
	row, err := db.SetPinned("/p/000", true, time.UnixMilli(5000))
	if err != nil || row.PinnedAt == nil || *row.PinnedAt != 5000 {
		t.Fatalf("pin = %+v, %v", row.PinnedAt, err)
	}
	listed, err := db.ListProjects("host")
	if err != nil || len(listed) != maxProjects+1 || listed[len(listed)-1].Path != "/p/000" {
		t.Fatalf("listed %d, last %s (%v)", len(listed), listed[len(listed)-1].Path, err)
	}
	if row, _ := db.SetPinned("/p/000", false, time.Now()); row.PinnedAt != nil {
		t.Fatal("unpinning left the pin")
	}
	if listed, _ := db.ListProjects("host"); len(listed) != maxProjects {
		t.Fatalf("listed %d after unpinning, want %d", len(listed), maxProjects)
	}
}
