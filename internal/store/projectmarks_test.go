package store

import (
	"errors"
	"fmt"
	"math"
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
	if row, _ := db.Rename("/src/codev2", "  "); row.DisplayName != "repogo" {
		t.Fatalf("blank rename = %q, want the repository back", row.DisplayName)
	}
	if _, err := db.Rename("/not/listed", "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("renaming an unlisted folder = %v, want ErrInvalid", err)
	}
}

// A pinned project is listed even past the cap, on top, and unpinning clears it.
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
	moved, err := db.Pin("/p/000", "")
	if err != nil || len(moved) != 1 || moved[0].PinOrder == nil {
		t.Fatalf("pin = %+v, %v", moved, err)
	}
	listed, err := db.ListProjects("host")
	if err != nil || len(listed) != maxProjects+1 || listed[0].Path != "/p/000" {
		t.Fatalf("listed %d, first %s (%v)", len(listed), listed[0].Path, err)
	}
	if row, _ := db.Unpin("/p/000"); row.PinOrder != nil {
		t.Fatal("unpinning left the pin")
	}
	if listed, _ := db.ListProjects("host"); len(listed) != maxProjects {
		t.Fatalf("listed %d after unpinning, want %d", len(listed), maxProjects)
	}
}

// pinned is the pinned paths as ListProjects orders them.
func pinned(t *testing.T, db *Store) []string {
	t.Helper()
	listed, err := db.ListProjects("host")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, row := range listed {
		if row.PinOrder != nil {
			paths = append(paths, row.Path)
		}
	}
	return paths
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

// A new pin goes on top; a move writes only the moved row.
func TestPinsGoOnTopAndAMoveWritesOneRow(t *testing.T) {
	db := newStore(t)
	syncFolders(t, db, "/a", "/b", "/c", "/d")
	for _, path := range []string{"/a", "/b", "/c"} {
		if _, err := db.Pin(path, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := pinned(t, db); !slices.Equal(got, []string{"/c", "/b", "/a"}) {
		t.Fatalf("pins = %v, want newest on top", got)
	}

	moved, err := db.Pin("/a", "/c")
	if err != nil || len(moved) != 1 || moved[0].Path != "/a" {
		t.Fatalf("move = %+v, %v; want only /a", moved, err)
	}
	if got := pinned(t, db); !slices.Equal(got, []string{"/c", "/a", "/b"}) {
		t.Fatalf("pins = %v, want /c /a /b", got)
	}

	// The end of the list, then the top; an after no longer pinned is the top.
	if _, err := db.Pin("/c", "/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pin("/b", "/d"); err != nil {
		t.Fatal(err)
	}
	if got := pinned(t, db); !slices.Equal(got, []string{"/b", "/a", "/c"}) {
		t.Fatalf("pins = %v, want /b /a /c", got)
	}
	if row, _ := db.Project("/d"); row.PinOrder != nil {
		t.Fatal("a move pinned its unpinned after")
	}

	if _, err := db.Unpin("/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pin("/a", ""); err != nil {
		t.Fatal(err)
	}
	if got := pinned(t, db); !slices.Equal(got, []string{"/a", "/b", "/c"}) {
		t.Fatalf("repinned = %v, want /a on top", got)
	}
	if _, err := db.Pin("/missing", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unlisted path = %v, want ErrInvalid", err)
	}
}

// Neighbours with no number between them renumber every pin, in order.
func TestAPinWithNoRoomRenumbers(t *testing.T) {
	db := newStore(t)
	syncFolders(t, db, "/a", "/b", "/c")
	tight := map[string]float64{"/a": 1, "/b": math.Nextafter(1, 2)}
	for path, order := range tight {
		if _, err := db.db.Exec(`INSERT INTO state.project_marks (path, pinned_at) VALUES (?, ?)`, path, order); err != nil {
			t.Fatal(err)
		}
	}
	moved, err := db.Pin("/c", "/a")
	if err != nil || len(moved) != 3 || moved[0].Path != "/c" {
		t.Fatalf("moved = %+v, %v; want all three, /c first", moved, err)
	}
	if got := pinned(t, db); !slices.Equal(got, []string{"/a", "/c", "/b"}) {
		t.Fatalf("pins = %v, want /a /c /b", got)
	}
	for i, path := range []string{"/a", "/c", "/b"} {
		if row, _ := db.Project(path); *row.PinOrder != float64(i) {
			t.Fatalf("%s = %v, want %d", path, *row.PinOrder, i)
		}
	}
}

// Unpinned projects follow the pins: the used ones by their last prompt or a
// later pick, then the never-used ones by activity.
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
	if _, err := db.Pin("/pinned", ""); err != nil {
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
	if want := []string{"/pinned", "/picked", "/prompted", "/fresh"}; !slices.Equal(paths, want) {
		t.Fatalf("listed %v, want %v", paths, want)
	}
	if listed[1].UsedAt != 5000 || listed[2].UsedAt != 3000 || listed[3].UsedAt != 0 {
		t.Fatalf("used_at = %d, %d, %d; want 5000, 3000, 0", listed[1].UsedAt, listed[2].UsedAt, listed[3].UsedAt)
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
