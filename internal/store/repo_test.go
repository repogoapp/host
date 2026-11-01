package store

import "testing"

// A pass that did not read a checkout keeps its first commit, the same way it
// keeps its repository, and a known repository needs both to be skipped.
func TestRootCommitIsKeptAndMakesARepoKnown(t *testing.T) {
	db := newStore(t)
	sync := func(rows ...Project) {
		t.Helper()
		if _, err := db.SyncProjects(rows); err != nil {
			t.Fatal(err)
		}
	}
	sync(Project{Path: "/a", Kind: ProjectClone, RepoOwner: "octocat", RepoName: "hello", RootCommit: "abc"},
		Project{Path: "/b", Kind: ProjectClone, RepoOwner: "octocat", RepoName: "world"})
	// The next pass looked at neither.
	sync(Project{Path: "/a", Kind: ProjectClone}, Project{Path: "/b", Kind: ProjectClone})

	rows, err := db.ProjectsAt([]string{"/a"})
	if err != nil || len(rows) != 1 || rows[0].RootCommit != "abc" || rows[0].RepoName != "hello" {
		t.Fatalf("rows %+v, %v: want /a's root and repository kept", rows, err)
	}
	known, err := db.KnownRepos()
	if err != nil || !known["/a"] || known["/b"] {
		t.Fatalf("known %v, %v: want /a only, since /b has no root yet", known, err)
	}
}

// SetRepo writes a new origin and hands back the row; the same origin again
// writes nothing.
func TestSetRepoReportsOnlyAMove(t *testing.T) {
	db := newStore(t)
	if _, err := db.SyncProjects([]Project{{Path: "/a", Kind: ProjectClone, RepoOwner: "octocat", RepoName: "hello"}}); err != nil {
		t.Fatal(err)
	}
	if _, moved, err := db.SetRepo("/a", "octocat", "hello"); err != nil || moved {
		t.Fatalf("the same origin moved: %v, %v", moved, err)
	}
	row, moved, err := db.SetRepo("/a", "new-owner", "renamed")
	if err != nil || !moved || row.RepoOwner != "new-owner" || row.DisplayName != "renamed" {
		t.Fatalf("row %+v, moved %v, %v: want new-owner/renamed, named after it", row, moved, err)
	}
	if _, moved, err := db.SetRepo("/missing", "a", "b"); err != nil || moved {
		t.Fatalf("a path that is not a project moved: %v, %v", moved, err)
	}
}

// Every checkout of one repository, and only those.
func TestCheckoutsAreTheFoldersWithARoot(t *testing.T) {
	db := newStore(t)
	if _, err := db.SyncProjects([]Project{
		{Path: "/a", Kind: ProjectClone, RootCommit: "abc"},
		{Path: "/a-worktree", Kind: ProjectClone, RootCommit: "abc"},
		{Path: "/b", Kind: ProjectClone, RootCommit: "def"},
		{Path: "/notes", Kind: ProjectFolder},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.Checkouts("abc")
	if err != nil || len(got) != 2 {
		t.Fatalf("checkouts %v, %v: want /a and /a-worktree", got, err)
	}
	if got, _ := db.Checkouts(""); len(got) != 0 {
		t.Fatalf("an empty root matched %v", got)
	}
	if got, _ := db.Checkouts("zzz"); got == nil || len(got) != 0 {
		t.Fatalf("an unknown root: %v, want an empty list", got)
	}
}
