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

	row, _, err := db.Rename("/src/codev2", "  RepoGo   v2 ")
	if err != nil || row.DisplayName != "RepoGo v2" {
		t.Fatalf("rename = %q, %v", row.DisplayName, err)
	}
	if row, _, _ := db.Rename("/src/codev2", "  "); row.DisplayName != "repogo" {
		t.Fatalf("blank rename = %q, want the repository back", row.DisplayName)
	}
	if _, _, err := db.Rename("/not/listed", "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("renaming an unlisted folder = %v, want ErrInvalid", err)
	}
}

// A worktree is called what its repository's clone is, and renaming the clone
// renames the worktree with it; a worktree the user named keeps its name.
func TestAWorktreeTakesItsClonesName(t *testing.T) {
	db := newStore(t)
	if _, err := db.SyncProjects([]Project{
		{Path: "/src/RepoGo", Kind: ProjectClone, RepoOwner: "alice", RepoName: "repogo", ActivityAt: 1},
		{Path: "/src/.worktrees/alice/repogo/fix-a1b2c3", Kind: ProjectManaged, RepoOwner: "alice", RepoName: "repogo", ActivityAt: 2},
		{Path: "/src/by-hand", Kind: ProjectWorktree, RepoOwner: "alice", RepoName: "repogo", ActivityAt: 3},
		{Path: "/src/other", Kind: ProjectManaged, RepoOwner: "bob", RepoName: "other", ActivityAt: 4},
	}); err != nil {
		t.Fatal(err)
	}
	called := func(path string) string {
		row, err := db.Project(path)
		if err != nil {
			t.Fatal(err)
		}
		return row.DisplayName
	}
	if got := called("/src/.worktrees/alice/repogo/fix-a1b2c3"); got != "RepoGo" {
		t.Fatalf("worktree is called %q, want its clone's RepoGo", got)
	}
	// No clone of bob/other is listed, so its worktree is called by its repository.
	if got := called("/src/other"); got != "other" {
		t.Fatalf("worktree without a clone is called %q, want other", got)
	}

	row, inherited, err := db.Rename("/src/RepoGo", "Mobile")
	if err != nil || row.DisplayName != "Mobile" {
		t.Fatalf("rename = %q, %v", row.DisplayName, err)
	}
	if got := paths(inherited); !slices.Equal(got, []string{"/src/.worktrees/alice/repogo/fix-a1b2c3", "/src/by-hand"}) {
		t.Fatalf("renamed with the clone = %v", got)
	}
	if got := called("/src/by-hand"); got != "Mobile" {
		t.Fatalf("worktree after the clone's rename = %q, want Mobile", got)
	}

	// A worktree's own name wins, and the clone's next rename leaves it alone.
	if _, _, err := db.Rename("/src/by-hand", "Experiment"); err != nil {
		t.Fatal(err)
	}
	_, inherited, err = db.Rename("/src/RepoGo", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(inherited); !slices.Equal(got, []string{"/src/.worktrees/alice/repogo/fix-a1b2c3"}) {
		t.Fatalf("renamed with the clone = %v, want only the unnamed worktree", got)
	}
	if got := called("/src/by-hand"); got != "Experiment" {
		t.Fatalf("named worktree = %q, want Experiment", got)
	}
}

// A sync that removes a clone sends the worktrees it renamed, though their
// own rows did not move.
func TestASyncSendsTheWorktreesAClonesRemovalRenames(t *testing.T) {
	db := newStore(t)
	clone := Project{Path: "/src/RepoGo", Kind: ProjectClone, RepoOwner: "alice", RepoName: "repogo", ActivityAt: 1}
	worktree := Project{Path: "/src/wt", Kind: ProjectWorktree, RepoOwner: "alice", RepoName: "repogo", ActivityAt: 2}
	if _, err := db.SyncProjects([]Project{clone, worktree}); err != nil {
		t.Fatal(err)
	}
	change, err := db.SyncProjects([]Project{worktree})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(change.Removed, []string{"/src/RepoGo"}) || len(change.Changed) != 1 ||
		change.Changed[0].Path != "/src/wt" || change.Changed[0].DisplayName != "repogo" {
		t.Fatalf("change = %+v", change)
	}
}

func paths(rows []Project) []string {
	out := []string{}
	for _, p := range rows {
		out = append(out, p.Path)
	}
	slices.Sort(out)
	return out
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

// A deleted project's pick goes with its marks, so the gone folder stops
// being a root.
func TestForgetMarksDropsThePick(t *testing.T) {
	db := newStore(t)
	if err := db.Pick("/tmp/picked", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.ForgetMarks("/tmp/picked"); err != nil {
		t.Fatal(err)
	}
	if roots, _ := db.Roots(); len(roots) != 0 {
		t.Fatalf("roots after ForgetMarks = %v, want none", roots)
	}
}
