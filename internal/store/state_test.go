package store

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/sqlitedb"
)

// rebuildCache closes db, marks its cache as another schema's and reopens the
// directory, which deletes and rebuilds the cache as a schema change does.
func rebuildCache(t *testing.T, db *Store, dir string) *Store {
	t.Helper()
	db.Close()
	raw := sqlitedb.Open(filepath.Join(dir, CacheFile))
	if _, err := raw.Exec(`PRAGMA user_version = 7`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	return reopened
}

// What the user made lives in state.db, so a cache rebuild (any schema
// change) keeps resolves, voice names, picks and queued turns.
func TestStateSurvivesACacheRebuild(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, db, "a", 1000, 1)
	if err := db.SetResolved("claude:a", true, time.UnixMilli(2000)); err != nil {
		t.Fatal(err)
	}
	if err := db.Pick("/tmp/picked", time.UnixMilli(3000)); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveQueue("claude:a", []agent.StoredTurn{{TurnID: "t1", QueuedAt: time.UnixMilli(4000),
		Request: agent.TurnRequest{ChatID: "claude:a", Agent: agent.KindClaude, Prompt: "next"}}}); err != nil {
		t.Fatal(err)
	}
	before, err := db.Info("claude:a")
	if err != nil || before.VoiceHandle == nil {
		t.Fatalf("no voice handle before the rebuild: %+v, %v", before, err)
	}

	db = rebuildCache(t, db, dir)
	if _, err := db.Info("claude:a"); err == nil {
		t.Fatal("the cache was not rebuilt")
	}
	write(t, db, "a", 1000, 1)
	after, err := db.Info("claude:a")
	if err != nil {
		t.Fatal(err)
	}
	if after.ResolvedAt == nil || *after.ResolvedAt != 2000 {
		t.Errorf("resolve lost: %v", after.ResolvedAt)
	}
	if after.VoiceHandle == nil || after.VoiceHandle.Key != before.VoiceHandle.Key {
		t.Errorf("voice name changed: %+v, was %+v", after.VoiceHandle, before.VoiceHandle)
	}
	if after.QueuedCount != 1 || after.QueueRev != 1 {
		t.Errorf("queue on the row = %d at %d, want 1 at 1", after.QueuedCount, after.QueueRev)
	}
	if roots, _ := db.Roots(); !slices.Contains(roots, "/tmp/picked") {
		t.Errorf("pick lost: roots = %v", roots)
	}
	// Search is the cache's own, rebuilt with it.
	if page, err := db.Chats(ChatQuery{Search: "chat"}); err != nil || len(page.Chats) != 1 {
		t.Errorf("search after the rebuild = %+v, %v", page.Chats, err)
	}
}

// After a rebuild, chats come back a few at a time. One deleted meanwhile
// takes only its own state; a chat not yet re-imported keeps its resolve and
// its voice name, which a new chat cannot take.
func TestAPartialRebuildKeepsStateOfChatsNotYetImported(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"kept", "deleted", "later"} {
		write(t, db, id, int64(1000+i), 1)
		if err := db.SetResolved(ChatID("claude:"+id), true, time.UnixMilli(5000)); err != nil {
			t.Fatal(err)
		}
	}
	later, _ := db.Info("claude:later")

	db = rebuildCache(t, db, dir)
	write(t, db, "kept", 1000, 1)
	write(t, db, "deleted", 1001, 1)
	if err := db.Delete("claude:deleted"); err != nil {
		t.Fatal(err)
	}
	// A new chat is named while "later" is still missing from the cache.
	write(t, db, "new", 9000, 1)
	fresh, _ := db.Info("claude:new")
	if fresh.VoiceHandle == nil || fresh.VoiceHandle.Key == later.VoiceHandle.Key {
		t.Fatalf("new chat's voice name = %+v, must not be the absent chat's %s", fresh.VoiceHandle, later.VoiceHandle.Key)
	}

	write(t, db, "later", 1002, 1)
	back, err := db.Info("claude:later")
	if err != nil {
		t.Fatal(err)
	}
	if back.ResolvedAt == nil || back.VoiceHandle == nil || back.VoiceHandle.Key != later.VoiceHandle.Key {
		t.Fatalf("a chat imported late lost its state: %+v", back)
	}
	write(t, db, "deleted", 1001, 1)
	if gone, _ := db.Info("claude:deleted"); gone.ResolvedAt != nil {
		t.Fatal("a deleted chat's resolve came back with it")
	}
}

// Resolving then unresolving leaves no row, and a conditional unresolve
// writes nothing when its condition fails.
func TestChatMarksWriteOnlyWhatChanged(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 1000, 1)
	var told int
	db.Notify(func(Change) { told++ })

	if err := db.SetResolved("claude:a", true, time.UnixMilli(5000)); err != nil {
		t.Fatal(err)
	}
	if cleared, err := db.UnresolveIfActive("claude:a", time.UnixMilli(4000)); err != nil || cleared {
		t.Fatalf("a prompt older than the resolve cleared it: %v, %v", cleared, err)
	}
	if told != 1 {
		t.Fatalf("told %d changes, want 1: a change that wrote nothing is not news", told)
	}
	if cleared, err := db.UnresolveIfActive("claude:a", time.UnixMilli(6000)); err != nil || !cleared {
		t.Fatalf("a newer prompt did not clear the resolve: %v, %v", cleared, err)
	}
	var rows int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM state.chat_marks`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("chat_marks holds %d rows after clearing, want 0 (%v)", rows, err)
	}
}

// A project's marks are one row: clearing one leaves the others, and the row
// goes once nothing is set.
func TestProjectMarksKeepEachOther(t *testing.T) {
	db := newStore(t)
	if err := db.Pick("/tmp/p", time.UnixMilli(1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.updateProjectMarks("/tmp/p", func(m *ProjectMarks) bool {
		m.Name = ptr("Mine")
		return true
	}); err != nil {
		t.Fatal(err)
	}
	unpick := func(m *ProjectMarks) bool { m.PickedAt = nil; return true }
	if _, err := db.updateProjectMarks("/tmp/p", unpick); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := db.db.QueryRow(`SELECT name FROM state.project_marks WHERE path = '/tmp/p'`).Scan(&name); err != nil || name != "Mine" {
		t.Fatalf("name after unpicking = %q, %v", name, err)
	}
	if _, err := db.updateProjectMarks("/tmp/p", func(m *ProjectMarks) bool { m.Name = nil; return true }); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM state.project_marks`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("project_marks holds %d rows with nothing set, want 0 (%v)", rows, err)
	}
}

// A chat's queue round-trips through state.db, its row carries the count and
// a version that moves on every save, the empty one included.
func TestSaveQueueFeedsTheRowAndChatsQueue(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 1000, 1)
	var changed []ChatID
	db.Notify(func(c Change) { changed = append(changed, c.Changed...) })

	shot := agent.Attachment{Name: "shot.png", MimeType: "image/png", Path: "/tmp/shot.png"}
	turns := []agent.StoredTurn{
		{TurnID: "t1", QueuedAt: time.UnixMilli(2000), Request: agent.TurnRequest{ChatID: "claude:a", Agent: agent.KindClaude,
			Prompt: "first", Config: agent.TurnConfig{Model: "opus"}, Attachments: []agent.Attachment{shot}}},
		{TurnID: "t2", QueuedAt: time.UnixMilli(3000), Held: true, Request: agent.TurnRequest{ChatID: "claude:a", Agent: agent.KindClaude, Prompt: "second"}},
	}
	if err := db.SaveQueue("claude:a", turns); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []ChatID{"claude:a"}) {
		t.Fatalf("changed = %v, want the chat's row", changed)
	}
	q, err := db.Queue("claude:a")
	if err != nil {
		t.Fatal(err)
	}
	if q.QueueRev != 1 || len(q.Queued) != 2 || q.Queued[0].Prompt != "first" || len(q.Queued[0].Attachments) != 1 ||
		q.Queued[0].Attachments[0] != shot || !q.Queued[1].QueuedAt.Equal(time.UnixMilli(3000)) {
		t.Fatalf("chats.queue = %+v", q)
	}
	loaded, err := db.LoadQueues()
	if err != nil || len(loaded["claude:a"]) != 2 || !loaded["claude:a"][1].Held || loaded["claude:a"][0].Request.Config.Model != "opus" {
		t.Fatalf("LoadQueues = %+v, %v", loaded, err)
	}

	if err := db.SaveQueue("claude:a", nil); err != nil {
		t.Fatal(err)
	}
	row, _ := db.Info("claude:a")
	if row.QueuedCount != 0 || row.QueueRev != 2 {
		t.Fatalf("row after emptying = %d at %d, want 0 at 2", row.QueuedCount, row.QueueRev)
	}
	if q, _ := db.Queue("claude:a"); q.Queued == nil || q.QueueRev != 2 {
		t.Fatalf("empty queue = %+v, want [] at 2", q)
	}

	// A new chat's queue under its temporary id has no row to announce.
	changed = nil
	if err := db.SaveQueue("temp-1", turns[:1]); err != nil || len(changed) != 0 {
		t.Fatalf("temporary id: err %v, changed %v", err, changed)
	}
}

// state.sql runs on every open: twice changes nothing, a column it gained is
// added to an older file with its rows kept, and a column that differs, or
// one state.sql lacks, stops the open.
func TestOpenStateAddsColumnsAndRefusesDifferences(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, StateFile)
	if err := openState(path); err != nil {
		t.Fatal(err)
	}
	if err := openState(path); err != nil {
		t.Fatalf("second open: %v", err)
	}

	older := filepath.Join(t.TempDir(), StateFile)
	raw := sqlitedb.Open(older)
	if _, err := raw.Exec(`CREATE TABLE project_marks (path TEXT PRIMARY KEY, picked_at INTEGER) WITHOUT ROWID;
		INSERT INTO project_marks VALUES ('/tmp/p', 1000)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if err := openState(older); err != nil {
		t.Fatalf("open an older file: %v", err)
	}
	db, err := Open(filepath.Dir(older))
	if err != nil {
		t.Fatal(err)
	}
	var pinned *int64
	var picked int64
	if err := db.db.QueryRow(`SELECT picked_at, pinned_at FROM state.project_marks WHERE path = '/tmp/p'`).Scan(&picked, &pinned); err != nil ||
		picked != 1000 || pinned != nil {
		t.Fatalf("older row = %d, %v (%v)", picked, pinned, err)
	}
	db.Close()

	for name, table := range map[string]string{
		"retyped":     `CREATE TABLE chat_queues (chat_id TEXT PRIMARY KEY, rev TEXT NOT NULL) WITHOUT ROWID`,
		"extra":       `CREATE TABLE chat_queues (chat_id TEXT PRIMARY KEY, rev INTEGER NOT NULL, stale INTEGER) WITHOUT ROWID`,
		"stray table": `CREATE TABLE dropped_feature (id INTEGER)`,
	} {
		path := filepath.Join(t.TempDir(), StateFile)
		raw := sqlitedb.Open(path)
		if _, err := raw.Exec(table); err != nil {
			t.Fatal(err)
		}
		raw.Close()
		if err := openState(path); err == nil {
			t.Errorf("%s: open succeeded, want refused", name)
		}
	}
}

// The attach survives a home folder with an apostrophe in it.
func TestStateAttachesFromAPathWithAnApostrophe(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jo's home")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Pick("/tmp/p", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// Each state table has one writer: no other file in the package
// inserts, updates or deletes it.
func TestStateTablesHaveOneWriter(t *testing.T) {
	writers := map[string][]string{
		"chat_marks":    {"marks.go", "import.go", "delete.go"},
		"project_marks": {"projectmarks.go"},
		"voice_handles": {"voice.go", "delete.go"},
		"queued_turns":  {"queue.go"},
		"chat_queues":   {"queue.go"},
		"schedules":     {"schedules.go"},
		"action_runs":   {"actionruns.go"},
	}
	shape, err := stateShape()
	if err != nil {
		t.Fatal(err)
	}
	for table := range shape {
		if _, ok := writers[table]; !ok {
			t.Errorf("state table %s has no writer listed here", table)
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for table, allowed := range writers {
			write := regexp.MustCompile(`(?i)(INSERT INTO|UPDATE|DELETE FROM)\s+state\.` + table + `\b`)
			if write.Match(src) && !slices.Contains(allowed, file) {
				t.Errorf("%s writes state.%s; only %v may", file, table, allowed)
			}
		}
	}
}
