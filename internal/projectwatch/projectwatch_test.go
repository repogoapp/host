package projectwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/git"
	"github.com/repogo/host/internal/testwait"
)

// The point of this package is that N subscribers cost one watcher, one git
// invocation, and one marshal — so that is what most of this asserts.

type fakeGit struct {
	mu     sync.Mutex
	calls  int
	status git.Status
	diff   git.ChangeSet
	// ignored is what git ignores; ignoreErr fails check-ignore.
	ignored   map[string]bool
	ignoreErr error
	asked     [][]string
}

// Contain refuses /etc and resolves /var as macOS does, to /private/var.
func (f *fakeGit) Contain(path string) (string, error) {
	if strings.HasPrefix(path, "/etc") {
		return "", errors.New("outside roots")
	}
	if strings.HasPrefix(path, "/var/") {
		return "/private" + path, nil
	}
	return path, nil
}

func (f *fakeGit) WorkingDiff(context.Context, string) (*git.ChangeSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	set := f.diff
	return &set, nil
}

func (f *fakeGit) Status(_ context.Context, paths []string) ([]git.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	out := f.status
	out.Path = paths[0]
	return []git.Status{out}, nil
}

func (f *fakeGit) Ignored(_ context.Context, _ string, paths []string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, paths)
	if f.ignoreErr != nil {
		return nil, f.ignoreErr
	}
	out := map[string]bool{}
	for _, p := range paths {
		if f.ignored[p] {
			out[p] = true
		}
	}
	return out, nil
}

func (f *fakeGit) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakePub struct {
	mu   sync.Mutex
	sent map[device.ID]int
	last []byte
	// files is every fs.changed payload, in order.
	files [][]byte
}

func newPub() *fakePub { return &fakePub{sent: map[device.ID]int{}} }

func (p *fakePub) Send(to device.ID, method string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if method == (FilesChanged{}).Method() {
		p.files = append(p.files, payload)
		return nil
	}
	p.sent[to]++
	p.last = payload
	return nil
}

func (p *fakePub) filePushes() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.files...)
}

func (p *fakePub) count(id device.ID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent[id]
}

func (p *fakePub) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.sent {
		n += c
	}
	return n
}

func newManager(g Git) *Manager {
	return New(Deps{Git: g, Totals: fakeTotals{}, Pushes: newPub(), Active: func(string, bool) {}, Log: slog.New(slog.DiscardHandler)})
}

func TestThreeDevicesOnOneProjectShareOneRoom(t *testing.T) {
	m := newManager(&fakeGit{})
	for _, id := range []device.ID{"a", "b", "c"} {
		if err := m.Watch(id, []string{"/proj"}, nil, false); err != nil {
			t.Fatalf("Watch(%s): %v", id, err)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.rooms) != 1 {
		t.Fatalf("rooms = %d, want 1 — a room per subscriber is the shape this package exists to avoid", len(m.rooms))
	}
	if got := len(m.rooms["/proj"].subs); got != 3 {
		t.Errorf("subs = %d, want 3", got)
	}
}

func TestPollRunsGitOnceAndPushesToEveryone(t *testing.T) {
	g := &fakeGit{status: git.Status{Repo: true, Branch: "main"}}
	m := newManager(g)
	pub := m.pub.(*fakePub)

	// A room without its watcher goroutine, so this poll is the only one.
	m.rooms["/proj"] = &room{subs: map[device.ID]struct{}{"a": {}, "b": {}, "c": {}}, cancel: func() {}}
	m.poll(context.Background(), "/proj")

	if g.count() != 1 {
		t.Errorf("git ran %d times for one poll, want 1", g.count())
	}
	if pub.total() != 3 {
		t.Errorf("pushes = %d, want 3 — one per subscriber", pub.total())
	}
}

// The dedupe is what makes the poll rate a private detail: an idle repository
// is silent no matter how often it is checked.
func TestUnchangedStatusIsNotPushedAgain(t *testing.T) {
	g := &fakeGit{status: git.Status{Repo: true, Branch: "main"}}
	m := newManager(g)
	pub := m.pub.(*fakePub)
	m.rooms["/proj"] = &room{subs: map[device.ID]struct{}{"a": {}}, cancel: func() {}}

	m.poll(context.Background(), "/proj")
	m.poll(context.Background(), "/proj")
	m.poll(context.Background(), "/proj")
	if pub.total() != 1 {
		t.Errorf("pushes = %d, want 1 — an unchanged project should be silent", pub.total())
	}

	g.mu.Lock()
	g.status.Branch = "feature"
	g.mu.Unlock()
	m.poll(context.Background(), "/proj")
	if pub.total() != 2 {
		t.Errorf("pushes = %d, want 2 — a branch change must land", pub.total())
	}
}

func TestTheLastSubscriberOutStopsTheWatcher(t *testing.T) {
	m := newManager(&fakeGit{})
	_ = m.Watch("a", []string{"/proj"}, nil, false)
	_ = m.Watch("b", []string{"/proj"}, nil, false)

	m.Stop("a")
	m.mu.Lock()
	if len(m.rooms) != 1 {
		t.Fatal("room closed while a subscriber remained")
	}
	m.mu.Unlock()

	m.Stop("b")
	if !idle(m, "/proj") {
		t.Error("the last subscriber left but the room is not lingering to stop")
	}
}

// idle reports whether a room has no devices and is counting down its linger.
func idle(m *Manager, path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[path]
	return ok && len(r.subs) == 0 && r.idle != nil
}

// A folder's watcher outlives its last device by the linger, then stops.
func TestAnIdleWatcherStopsAfterItsLinger(t *testing.T) {
	m := newManager(&fakeGit{})
	m.linger = 20 * time.Millisecond
	_ = m.Watch("a", []string{"/proj"}, nil, false)
	m.Stop("a")

	testwait.For(t, "the watcher to stop after its linger", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		_, running := m.rooms["/proj"]
		return !running
	})
}

// A device back inside the linger keeps the same watcher, which polls again.
func TestComingBackInsideTheLingerResumesTheWatcher(t *testing.T) {
	g := &fakeGit{status: git.Status{Repo: true, Branch: "main"}}
	m := newManager(g)
	m.linger = time.Hour
	_ = m.Watch("a", []string{"/proj"}, nil, false)
	m.mu.Lock()
	first := m.rooms["/proj"]
	m.mu.Unlock()

	m.Stop("a")
	_ = m.Watch("a", []string{"/proj"}, nil, false)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rooms["/proj"] != first {
		t.Fatal("coming back inside the linger started a second watcher")
	}
	if first.idle != nil {
		t.Error("the resumed room is still counting down")
	}
}

// A lingering room runs no git: nobody would hear it.
func TestAnIdleRoomDoesNotPoll(t *testing.T) {
	g := &fakeGit{status: git.Status{Repo: true}}
	m := newManager(g)
	m.linger = time.Hour
	m.mu.Lock()
	m.rooms["/proj"] = &room{subs: map[device.ID]struct{}{}, cancel: func() {}}
	m.mu.Unlock()
	m.poll(context.Background(), "/proj")
	if g.count() != 0 {
		t.Errorf("git ran %d times for an idle room, want 0", g.count())
	}
}

// Re-subscribing is how a client renews, and it happens whenever the sidebar
// scrolls. Restarting every watcher each time would defeat the whole design.
func TestResubscribeOnlyMovesWhatChanged(t *testing.T) {
	m := newManager(&fakeGit{})
	_ = m.Watch("a", []string{"/one", "/two"}, nil, false)

	m.mu.Lock()
	kept := m.rooms["/two"]
	m.mu.Unlock()

	_ = m.Watch("a", []string{"/two", "/three"}, nil, false)

	if !idle(m, "/one") {
		t.Error("/one was dropped from the set but its watcher is not winding down")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rooms["/two"] != kept {
		t.Error("/two was in both sets and should not have been restarted")
	}
	if _, ok := m.rooms["/three"]; !ok {
		t.Error("/three was added but has no watcher")
	}
}

// The host cannot see a client disconnect, so the lease is the only thing
// standing between a vanished phone and a watcher that runs forever.
func TestAnExpiredLeaseDropsTheSubscription(t *testing.T) {
	m := newManager(&fakeGit{})
	_ = m.Watch("gone", []string{"/proj"}, nil, false)
	_ = m.Watch("here", []string{"/other"}, nil, false)

	m.mu.Lock()
	m.leases["gone"] = time.Now().Add(-time.Minute)
	m.mu.Unlock()
	m.sweep()

	if !idle(m, "/proj") {
		t.Error("the expired device's watcher is not winding down")
	}
	if idle(m, "/other") {
		t.Error("a live device's watcher was swept")
	}
}

func TestSubscriptionsAreBounded(t *testing.T) {
	paths := make([]string, MaxPaths+1)
	for i := range paths {
		paths[i] = filepath.Join("/proj", string(rune('a'+i%26)))
	}
	if err := newManager(&fakeGit{}).Watch("a", paths, nil, false); !errors.Is(err, ErrTooManyPaths) {
		t.Errorf("Watch = %v, want ErrTooManyPaths", err)
	}
}

// A worktree — one per chat, which is where the host is going — keeps its metadata
// elsewhere and leaves a pointer file behind. Following it is what keeps the
// cheap branch check working there at all.
func TestFingerprintFollowsAWorktreePointer(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real-git-dir")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, ".git"), []byte("gitdir: "+real+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := fingerprint(tree)
	if err := os.WriteFile(filepath.Join(real, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fingerprint(tree) == before {
		t.Error("HEAD moved in the worktree's real git dir and the fingerprint did not change")
	}
}

// An ordinary checkout keeps .git as a directory, and must not be sent through
// the pointer path.
func TestFingerprintOnAPlainCheckout(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	before := fingerprint(root)
	if err := os.WriteFile(filepath.Join(root, ".git", "index"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fingerprint(root) == before {
		t.Error("the index changed and the fingerprint did not")
	}
}

type fakeTotals struct{}

func (fakeTotals) UpdateProjectDiff(string, bool, int, int, int) (bool, error) { return false, nil }

// The change chip reads its numbers off the push, as v1's git.diff_stats did,
// so editing an already-changed file must push even though no file count moved.
func TestPushCarriesTheTotalsAndMovesWithThem(t *testing.T) {
	g := &fakeGit{status: git.Status{Repo: true, Unstaged: 1}, diff: git.ChangeSet{Kind: "working", FilesChanged: 1, Additions: 3}}
	m := newManager(g)
	pub := m.pub.(*fakePub)
	m.mu.Lock()
	m.rooms["/proj"] = &room{subs: map[device.ID]struct{}{"a": {}}, cancel: func() {}}
	m.mu.Unlock()

	m.poll(context.Background(), "/proj")
	var got Changed
	if err := json.Unmarshal(pub.last, &got); err != nil || len(got.Projects) != 1 || got.Projects[0].Working == nil {
		t.Fatalf("push = %s, want one project with working totals", pub.last)
	}
	if w := got.Projects[0].Working; w.FilesChanged != 1 || w.Additions != 3 {
		t.Errorf("working = %+v, want 1 file, +3", *w)
	}

	g.mu.Lock()
	g.diff.Additions = 10
	g.mu.Unlock()
	m.poll(context.Background(), "/proj")
	if pub.total() != 2 {
		t.Errorf("pushes = %d, want 2 — new line counts in the same file must land", pub.total())
	}
}

// A running room is silent until something moves, so a second device would
// otherwise wait for the next edit to see anything.
func TestJoiningARunningRoomGetsItsLastPush(t *testing.T) {
	m := newManager(&fakeGit{status: git.Status{Repo: true, Branch: "main"}})
	pub := m.pub.(*fakePub)
	_ = m.Watch("a", []string{"/proj"}, nil, false)
	m.poll(context.Background(), "/proj")

	_ = m.Watch("b", []string{"/proj"}, nil, false)
	if pub.count("b") != 1 {
		t.Errorf("b got %d pushes on joining, want 1", pub.count("b"))
	}
	_ = m.Watch("b", []string{"/proj"}, nil, false)
	if pub.count("b") != 1 {
		t.Errorf("b got %d pushes after renewing, want still 1 — a renewal is not a join", pub.count("b"))
	}
}

func TestResyncResendsWhatAJoinedDeviceWasSent(t *testing.T) {
	m := newManager(&fakeGit{status: git.Status{Repo: true, Branch: "main"}})
	pub := m.pub.(*fakePub)
	_ = m.Watch("a", []string{"/proj", "/other"}, nil, false)
	m.poll(context.Background(), "/proj")
	m.poll(context.Background(), "/other")
	if pub.count("a") != 2 {
		t.Fatalf("a got %d pushes from the first reads, want 2", pub.count("a"))
	}

	// A relaunch inside the lease: already joined, so a plain renewal is silent.
	_ = m.Watch("a", []string{"/proj", "/other"}, nil, false)
	if pub.count("a") != 2 {
		t.Fatalf("a got %d pushes after renewing, want still 2", pub.count("a"))
	}
	_ = m.Watch("a", []string{"/proj", "/other"}, nil, true)
	if pub.count("a") != 4 {
		t.Errorf("a got %d pushes after resync, want 4 — one per watched path", pub.count("a"))
	}

	// A path still on its first read has nothing to resend; its read pushes.
	_ = m.Watch("a", []string{"/proj", "/other", "/new"}, nil, true)
	if pub.count("a") != 6 {
		t.Errorf("a got %d pushes after resync with a new path, want 6", pub.count("a"))
	}
}

func TestRelevantKeepsWhatMovesTheDiff(t *testing.T) {
	const root, gitdir = "/r", "/r/.git"
	for path, want := range map[string]bool{
		"/r/main.go":                   true,
		"/r/src/app.ts":                true,
		"/r/.git/HEAD":                 true,
		"/r/.git/index":                true,
		"/r/.git/refs/heads/main":      true,
		"/r/.git/objects/ab/cdef":      false,
		"/r/.git/logs/HEAD":            false,
		"/r/node_modules/x/index.js":   false,
		"/r/apps/web/.next/cache/file": false,
		"/r/.DS_Store":                 false,
		"/other/main.go":               false,
	} {
		if got := relevant(root, gitdir, path); got != want {
			t.Errorf("relevant(%q) = %v, want %v", path, got, want)
		}
	}
}

// A worktree's metadata lives outside the checkout, and HEAD there still counts.
func TestRelevantFollowsAWorktreeGitDir(t *testing.T) {
	if !relevant("/tree", "/repo/.git/worktrees/tree", "/repo/.git/worktrees/tree/HEAD") {
		t.Error("HEAD in the worktree's git dir was ignored")
	}
}

// activeLog records OnActive calls, which run on the subscriber's goroutine.
type activeLog struct {
	mu     sync.Mutex
	events []string
}

func (w *activeLog) record(path string, active bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if active {
		w.events = append(w.events, "+"+path)
	} else {
		w.events = append(w.events, "-"+path)
	}
}

func (w *activeLog) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.events
	w.events = nil
	return out
}

// The environment runner starts a project's services while a device has it in
// front of the user and counts down once none does, so it must hear the first
// active device and the last, and nothing for paths only watched.
func TestOnActiveReportsTheFirstActiveDeviceAndTheLast(t *testing.T) {
	m := newManager(&fakeGit{})
	log := &activeLog{}
	m.onActive = log.record

	_ = m.Watch("a", []string{"/proj", "/bg"}, []string{"/proj"}, false)
	_ = m.Watch("b", []string{"/proj"}, []string{"/proj"}, false)
	_ = m.Watch("a", []string{"/proj", "/bg"}, []string{"/proj"}, false) // a renewal
	if got := log.take(); len(got) != 1 || got[0] != "+/proj" {
		t.Fatalf("events = %v, want one +/proj", got)
	}

	// Active must be watched: a path only named in active is ignored.
	_ = m.Watch("c", []string{"/other"}, []string{"/nowhere"}, false)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("events = %v for an unwatched active path", got)
	}

	m.Stop("a")
	if got := log.take(); len(got) != 0 {
		t.Fatalf("events = %v while b still has /proj active", got)
	}

	// Still watched, no longer active: the last device let go.
	_ = m.Watch("b", []string{"/proj", "/other"}, []string{"/other"}, false)
	got := log.take()
	if len(got) != 2 || got[0] != "-/proj" || got[1] != "+/other" {
		t.Fatalf("events = %v, want [-/proj +/other]", got)
	}

	// Lease expiry is the last way out.
	m.mu.Lock()
	m.leases["b"] = time.Now().Add(-time.Minute)
	m.mu.Unlock()
	m.sweep()
	if got := log.take(); len(got) != 1 || got[0] != "-/other" {
		t.Fatalf("events = %v, want [-/other] after the lease lapsed", got)
	}
}

// A push names its project by the watched path and the phone routes it by its
// own cwd, so the watch keeps the device's spelling; a path outside every root
// is dropped rather than failing the rest.
func TestWatchKeepsTheDevicesContainedPaths(t *testing.T) {
	m := newManager(&fakeGit{})
	if err := m.Watch("a", []string{"/var/proj", "/etc/nope"}, []string{"/var/proj", "/etc/nope"}, false); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.rooms) != 1 || m.rooms["/var/proj"] == nil {
		t.Errorf("rooms = %v, want only /var/proj as the device spelled it", m.rooms)
	}
	if len(m.active) != 1 || m.active["/var/proj"] == nil {
		t.Errorf("active = %v, want only /var/proj", m.active)
	}
}

// An editor re-reads the files it has open, so the push names the worktree
// file; git's own state and the checkout folder name none.
func TestWorktreeFileIsRelativeToTheCheckout(t *testing.T) {
	const root, gitdir = "/r", "/r/.git"
	for path, want := range map[string]string{
		"/r/main.go":    "main.go",
		"/r/src/app.ts": "src/app.ts",
		"/r/.git/index": "",
		"/r":            "",
	} {
		if got := worktreeFile(root, gitdir, path); got != want {
			t.Errorf("worktreeFile(%q) = %q, want %q", path, got, want)
		}
	}
	if got := worktreeFile("/tree", "/repo/.git/worktrees/tree", "/repo/.git/worktrees/tree/HEAD"); got != "" {
		t.Errorf("a worktree's git dir named %q, want none", got)
	}
}

func TestChangedFilesAreCollectedOnceEachAndCapped(t *testing.T) {
	var c changedFiles
	c.add("b.go", false)
	c.add("a.go", true)
	c.add("b.go", true)
	c.add("", true)
	files, truncated := c.take()
	if len(files) != 2 || !files["a.go"] || !files["b.go"] || truncated {
		t.Errorf("take() = %v, %v; want a.go and b.go, both created, not truncated", files, truncated)
	}
	if files, _ := c.take(); len(files) != 0 {
		t.Errorf("a second take() = %v, want empty", files)
	}

	for i := range maxCollected + 5 {
		c.add(fmt.Sprintf("f/%d", i), false)
	}
	files, truncated = c.take()
	if len(files) != maxCollected || !truncated {
		t.Errorf("take() after %d files = %d files, truncated %v; want %d, true", maxCollected+5, len(files), truncated, maxCollected)
	}
}

// A push names at most maxFiles; more is truncated, fewer keeps what the
// collection said.
func TestCappedCutsAtMaxFiles(t *testing.T) {
	many := make([]FileChange, maxFiles+1)
	if got, truncated := capped(many, false); len(got) != maxFiles || !truncated {
		t.Errorf("capped(%d) = %d, %v; want %d, true", len(many), len(got), truncated, maxFiles)
	}
	if got, truncated := capped(many[:3], false); len(got) != 3 || truncated {
		t.Errorf("capped(3) = %d, %v; want 3, false", len(got), truncated)
	}
	if _, truncated := capped(many[:3], true); !truncated {
		t.Error("capped dropped the collection's truncated")
	}
}

// Build output under an ignored folder never reaches the phone: the settle
// drops it, and later reports under the folder stop at add. An ignored file
// outside an ignored folder, such as .env, still syncs.
func TestIgnoredFoldersAreDroppedAndRememberedButIgnoredFilesSync(t *testing.T) {
	g := &fakeGit{ignored: map[string]bool{"build/": true, ".env": true}}
	m := newManager(g)
	var moved changedFiles
	for _, file := range []string{"build/out/app.o", "build/out/app.d", ".env", "src/app.go"} {
		moved.add(file, true)
	}
	files, _ := moved.take()

	kept := m.dropIgnored(context.Background(), "/proj", &moved, files)
	if len(kept) != 2 || !kept["src/app.go"] || !kept[".env"] {
		t.Errorf("kept = %v, want src/app.go and .env", kept)
	}
	if asked := g.asked[0]; !slices.Contains(asked, "build/") || !slices.Contains(asked, "build/out/") || slices.Contains(asked, ".env") {
		t.Errorf("asked %v, want only the folders above each file", asked)
	}

	moved.add("build/out/next.o", true)
	if files, _ := moved.take(); len(files) != 0 {
		t.Errorf("a file under a remembered ignored folder was collected: %v", files)
	}

	moved.add(".gitignore", false)
	moved.add("build/out/after.o", true)
	if files, _ := moved.take(); !files["build/out/after.o"] {
		t.Errorf("after .gitignore changed, collected %v; want the folder asked about again", files)
	}
}

// A settle of top-level files has no folders to ask about, so git is not run.
func TestTopLevelFilesSkipTheIgnoreCheck(t *testing.T) {
	g := &fakeGit{}
	m := newManager(g)
	var moved changedFiles
	kept := m.dropIgnored(context.Background(), "/proj", &moved, map[string]bool{".env": false, "README.md": false})
	if len(kept) != 2 || len(g.asked) != 0 {
		t.Errorf("kept %v after %d git runs; want both files and no run", kept, len(g.asked))
	}
}

func TestIgnoredCheckFailingKeepsEveryFile(t *testing.T) {
	m := newManager(&fakeGit{ignoreErr: errors.New("git broke")})
	var moved changedFiles
	files := map[string]bool{"build/out/app.o": true, "src/app.go": false}
	if kept := m.dropIgnored(context.Background(), "/proj", &moved, files); len(kept) != 2 {
		t.Errorf("kept = %v, want both files when git cannot answer", kept)
	}
}

// The kind comes from the disk at flush: gone is a delete whatever FSEvents
// said, a created file still there is a create, the rest are updates.
func TestClassifyNamesEachChangeFromTheDisk(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"kept.go", "new.go"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := classify(root, map[string]bool{"kept.go": false, "new.go": true, "gone.go": true})
	want := []FileChange{
		{File: "gone.go", Kind: KindDelete},
		{File: "kept.go", Kind: KindUpdate},
		{File: "new.go", Kind: KindCreate},
	}
	if !slices.Equal(got, want) {
		t.Errorf("classify = %v, want %v", got, want)
	}
}

// Every device in the room hears which files moved, even when the git status
// did not change; a settle that moved nothing sends nothing.
func TestFileChangesReachTheRoom(t *testing.T) {
	m := newManager(&fakeGit{})
	pub := m.pub.(*fakePub)
	_ = m.Watch("a", []string{"/proj"}, nil, false)
	_ = m.Watch("b", []string{"/proj"}, nil, false)

	m.sendChanges("/proj", nil, false)
	if n := len(pub.filePushes()); n != 0 {
		t.Fatalf("pushes = %d for no files, want 0", n)
	}

	m.sendChanges("/proj", []FileChange{{File: "src/app.ts", Kind: KindUpdate}}, false)
	pushes := pub.filePushes()
	if len(pushes) != 2 {
		t.Fatalf("pushes = %d, want 2 — one per device", len(pushes))
	}
	var got FilesChanged
	if err := json.Unmarshal(pushes[0], &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != "/proj" || len(got.Changes) != 1 || got.Changes[0] != (FileChange{File: "src/app.ts", Kind: KindUpdate}) || got.Truncated {
		t.Errorf("push = %+v", got)
	}

	m.sendChanges("/elsewhere", []FileChange{{File: "x", Kind: KindUpdate}}, false)
	if n := len(pub.filePushes()); n != 2 {
		t.Errorf("pushes = %d after a path nobody watches, want still 2", n)
	}
}
