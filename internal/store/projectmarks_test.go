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

	row, _, err := db.Rename("/src/codev2", "  RepoGo   v2 ", 100)
	if err != nil || row.DisplayName != "RepoGo v2" || row.Name != "RepoGo v2" || row.DefaultName != "repogo" {
		t.Fatalf("rename = %q (name %q), %v", row.DisplayName, row.Name, err)
	}
	if row, _, _ := db.Rename("/src/codev2", "  ", 200); row.DisplayName != "repogo" || row.Name != "" {
		t.Fatalf("blank rename = %q (name %q), want the repository back", row.DisplayName, row.Name)
	}
	if _, _, err := db.Rename("/not/listed", "x", 300); !errors.Is(err, ErrInvalid) {
		t.Fatalf("renaming an unlisted folder = %v, want ErrInvalid", err)
	}
}

// A pinned project is listed even past the cap, and unpinning lets it go.
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
	row, written, err := db.SetPinned("/p/000", true, 100)
	if err != nil || !written || !row.Pinned || row.PinnedUpdatedAt != 100 {
		t.Fatalf("pin = %+v, %v, %v", row, written, err)
	}
	listed, err := db.ListProjects("host")
	if err != nil || len(listed) != maxProjects+1 || !slices.ContainsFunc(listed, func(p Project) bool { return p.Path == "/p/000" }) {
		t.Fatalf("listed %d, want %d with /p/000 (%v)", len(listed), maxProjects+1, err)
	}
	if row, _, _ := db.SetPinned("/p/000", false, 200); row.Pinned || row.PinnedUpdatedAt != 200 {
		t.Fatalf("unpin = %+v", row)
	}
	if listed, _ := db.ListProjects("host"); len(listed) != maxProjects {
		t.Fatalf("listed %d after unpinning, want %d", len(listed), maxProjects)
	}
	if _, _, err := db.SetPinned("/missing", true, 300); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unlisted path = %v, want ErrInvalid", err)
	}
}

// A name or pin older than the stored one is ignored, whatever it says; an
// equal time is too, so a repeated write changes nothing.
func TestAnOlderMarkIsIgnored(t *testing.T) {
	db := newStore(t)
	syncFolders(t, db, "/a")

	if _, written, _ := db.Rename("/a", "New", 200); !written {
		t.Fatal("first rename not written")
	}
	row, written, err := db.Rename("/a", "Old", 100)
	if err != nil || written || row.Name != "New" || row.NameUpdatedAt != 200 {
		t.Fatalf("older rename = %q at %d, written %v (%v)", row.Name, row.NameUpdatedAt, written, err)
	}
	if _, written, _ := db.Rename("/a", "Same time", 200); written {
		t.Fatal("a rename at the stored time was written")
	}

	if _, written, _ := db.SetPinned("/a", false, 300); !written {
		t.Fatal("first unpin not written")
	}
	row, written, _ = db.SetPinned("/a", true, 250)
	if written || row.Pinned || row.PinnedUpdatedAt != 300 {
		t.Fatalf("older pin = %v at %d, written %v", row.Pinned, row.PinnedUpdatedAt, written)
	}
	// The name's time and the pin's are separate: the pin at 300 doesn't
	// outvote a rename at 250.
	if row, written, _ := db.Rename("/a", "Later", 250); !written || row.Name != "Later" {
		t.Fatalf("rename at 250 = %q, written %v", row.Name, written)
	}
}

// Clearing a name or unpinning keeps the time, so the row stays and an older
// write still loses.
func TestAClearKeepsItsTime(t *testing.T) {
	db := newStore(t)
	syncFolders(t, db, "/a")
	db.Rename("/a", "Named", 100)
	db.SetPinned("/a", true, 100)
	db.Rename("/a", "", 200)
	db.SetPinned("/a", false, 200)

	var name string
	var nameAt, pinnedAt int64
	if err := db.db.QueryRow(`SELECT name, name_updated_at, pinned_updated_at FROM state.project_marks WHERE path = '/a'`).
		Scan(&name, &nameAt, &pinnedAt); err != nil || name != "" || nameAt != 200 || pinnedAt != 200 {
		t.Fatalf("cleared row = %q, %d, %d (%v)", name, nameAt, pinnedAt, err)
	}
	if row, written, _ := db.Rename("/a", "Named", 150); written || row.DisplayName != "a" {
		t.Fatalf("an older name came back: %q, written %v", row.DisplayName, written)
	}
}

// A name or pin stored before marks had times reads as set, at time 0, and
// any timed change beats it.
func TestAMarkWithoutATimeLosesToAnyChange(t *testing.T) {
	db := newStore(t)
	syncFolders(t, db, "/a")
	if _, err := db.db.Exec(`INSERT INTO state.project_marks (path, pinned_at, name) VALUES ('/a', 2.5, 'Before')`); err != nil {
		t.Fatal(err)
	}
	row, _ := db.Project("/a")
	if row.Name != "Before" || row.NameUpdatedAt != 0 || !row.Pinned || row.PinnedUpdatedAt != 0 {
		t.Fatalf("untimed row = %+v", row)
	}
	if row, written, _ := db.Rename("/a", "After", 1); !written || row.Name != "After" {
		t.Fatalf("timed rename = %q, written %v", row.Name, written)
	}
	if row, written, _ := db.SetPinned("/a", false, 1); !written || row.Pinned {
		t.Fatalf("timed unpin = %v, written %v", row.Pinned, written)
	}
}

func syncFolders(t *testing.T, db *Store, paths ...string) {
	t.Helper()
	rows := make([]Project, len(paths))
	for i, path := range paths {
		rows[i] = Project{Path: path, Kind: ProjectFolder, ActivityAt: int64(i + 1)}
	}
	if _, err := db.SyncProjects(rows); err != nil {
		t.Fatal(err)
	}
}

// The list goes by use: the used ones by their last prompt or a later pick,
// then the never-used ones by activity. A pin doesn't move a project; devices
// order the pins themselves.
func TestTheListOrdersByUse(t *testing.T) {
	db := newStore(t)
	if _, err := db.SyncProjects([]Project{
		{Path: "/prompted", Kind: ProjectFolder, ActivityAt: 9000, LastMessageAt: 3000},
		{Path: "/fresh", Kind: ProjectFolder, ActivityAt: 4000},
		{Path: "/picked", Kind: ProjectFolder, ActivityAt: 1000},
		{Path: "/pinned", Kind: ProjectFolder, ActivityAt: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Pick("/picked", time.UnixMilli(5000)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.SetPinned("/pinned", true, 100); err != nil {
		t.Fatal(err)
	}
	listed, err := db.ListProjects("host")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, row := range listed {
		paths = append(paths, row.Path)
	}
	if want := []string{"/picked", "/prompted", "/fresh", "/pinned"}; !slices.Equal(paths, want) {
		t.Fatalf("listed %v, want %v", paths, want)
	}
	if listed[0].UsedAt != 5000 || listed[1].UsedAt != 3000 || listed[2].UsedAt != 0 {
		t.Fatalf("used_at = %d, %d, %d; want 5000, 3000, 0", listed[0].UsedAt, listed[1].UsedAt, listed[2].UsedAt)
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
