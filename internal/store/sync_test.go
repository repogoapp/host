package store

import (
	"testing"
)

// A restart keeps the epoch and the counter, so a device's cursor still holds:
// it pulls only what changed, even after the newest chat was deleted, whose
// revision no row shows any more.
func TestRestartKeepsTheCursorAfterDeletingTheNewestRow(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, db, "a", 100, 1)
	write(t, db, "b", 200, 1)
	held, err := db.Pull(PullRequest{Family: "chats"}, "host")
	if err != nil || len(held.Upsert) != 2 {
		t.Fatalf("first pull = %d rows, %v", len(held.Upsert), err)
	}
	if err := db.Delete("claude:b"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.Epoch() != held.Epoch {
		t.Fatal("a restart changed the epoch; every device would pull everything again")
	}
	write(t, db, "c", 300, 1)
	chat, err := db.Info("claude:c")
	if err != nil || chat.Rev <= held.Rev {
		t.Fatalf("new chat at rev %d, want past the held %d (%v)", chat.Rev, held.Rev, err)
	}
	next, err := db.Pull(PullRequest{Family: "chats", Epoch: held.Epoch, Since: held.Rev, IDs: []string{"claude:a", "claude:b"}}, "host")
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Upsert) != 1 || next.Upsert[0].ID != "claude:c" || len(next.Delete) != 1 || next.Delete[0] != "claude:b" {
		t.Fatalf("pull after restart = upsert %+v delete %v, want only c and b's removal", next.Upsert, next.Delete)
	}
}

// A rebuilt cache drops every row's revision, so it takes a new epoch and
// devices pull everything once.
func TestACacheRebuildChangesTheEpoch(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := db.Epoch()
	if db = rebuildCache(t, db, dir); db.Epoch() == before {
		t.Fatal("a rebuilt cache kept its epoch")
	}
}

// The list is last prompt first, else last activity, with path breaking ties,
// and every row names the host that served it.
func TestListProjectsOrdersByLastMessageThenActivity(t *testing.T) {
	db := newStore(t)
	if _, err := db.SyncProjects([]Project{
		{Path: "/old-chat", Kind: ProjectFolder, ActivityAt: 500, LastMessageAt: 100},
		{Path: "/fresh-folder", Kind: ProjectFolder, ActivityAt: 300},
		{Path: "/recent-chat", Kind: ProjectFolder, ActivityAt: 400, LastMessageAt: 400},
		{Path: "/tie-b", Kind: ProjectFolder, ActivityAt: 200},
		{Path: "/tie-a", Kind: ProjectFolder, ActivityAt: 200},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := db.ListProjects("host-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/recent-chat", "/fresh-folder", "/tie-a", "/tie-b", "/old-chat"}
	if len(projects) != len(want) {
		t.Fatalf("listed %d projects, want %d", len(projects), len(want))
	}
	for i, p := range projects {
		if p.Path != want[i] || p.HostID != "host-1" {
			t.Fatalf("row %d = %s on %q, want %s on host-1", i, p.Path, p.HostID, want[i])
		}
	}
}

// An empty host lists an empty array, not null, so a client decodes it as a list.
func TestListProjectsEmptyIsAnEmptyList(t *testing.T) {
	projects, err := newStore(t).ListProjects("host-1")
	if err != nil || projects == nil || len(projects) != 0 {
		t.Fatalf("empty list = %#v, %v", projects, err)
	}
}

// A pass reports the rows it moved, whole (diff totals included), and the
// paths it removed; an unchanged pass reports nothing, so nothing is sent.
func TestSyncProjectsReportsWhatMoved(t *testing.T) {
	db := newStore(t)
	first := []Project{
		{Path: "/a", Kind: ProjectFolder, ActivityAt: 100},
		{Path: "/b", Kind: ProjectFolder, ActivityAt: 200},
	}
	if change, err := db.SyncProjects(first); err != nil || len(change.Changed) != 2 || len(change.Removed) != 0 {
		t.Fatalf("first pass = %+v, %v", change, err)
	}
	if _, err := db.UpdateProjectDiff("/a", true, 3, 10, 2); err != nil {
		t.Fatal(err)
	}
	if change, err := db.SyncProjects(first); err != nil || !change.Empty() {
		t.Fatalf("unchanged pass = %+v, %v", change, err)
	}

	change, err := db.SyncProjects([]Project{{Path: "/a", Kind: ProjectFolder, ActivityAt: 300, LastMessageAt: 300}})
	if err != nil {
		t.Fatal(err)
	}
	if len(change.Changed) != 1 || change.Changed[0].Path != "/a" || change.Changed[0].ActivityAt != 300 ||
		change.Changed[0].FilesChanged != 3 || len(change.Removed) != 1 || change.Removed[0] != "/b" {
		t.Fatalf("moved pass = %+v", change)
	}
}
