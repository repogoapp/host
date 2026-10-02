// Package projectwatch runs one watcher per project folder, however many
// devices hold it, and pushes what moved there: fs.change for files and
// git.changed for the git state. FSEvents where it exists; polling elsewhere.
package projectwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/git"
)

const (
	// How often the fingerprint is checked when there is no tree watcher.
	tick = 3 * time.Second

	// settle coalesces a burst of writes into one git run and one fs.change.
	settle = 250 * time.Millisecond

	// backstop re-reads a tree watcher's project anyway: FSEvents can drop.
	backstop = time.Minute

	// full is how often a project gets a real `git status` regardless of the
	// fingerprint: editing src/ touches nothing under .git.
	full = 15 * time.Second

	// lease is how long a device's watch set outlives its last renewal; the
	// host cannot see a client disconnect through the relay.
	lease = 2 * time.Minute

	// linger keeps a folder's watcher, idle, after its last device leaves, so
	// switching projects and back does not rebuild it.
	linger = time.Minute

	// MaxPaths bounds one watch set. Matches git.MaxBatch: a client that asks
	// to watch more projects than it can show in a batch is asking for work
	// nobody will look at.
	MaxPaths = git.MaxBatch
)

var ErrTooManyPaths = errkind.New(errkind.Invalid, "projectwatch: too many paths in one watch")

// Git is the git service, narrowed to what a watcher runs.
type Git interface {
	Contain(path string) (string, error)
	Status(ctx context.Context, paths []string) ([]git.Status, error)
	WorkingDiff(ctx context.Context, path string) (*git.ChangeSet, error)
	Ignored(ctx context.Context, dir string, paths []string) (map[string]bool, error)
}

// gitFiles are what git writes when HEAD, the index, or a ref moves.
var gitFiles = []string{"HEAD", "index", "refs", "packed-refs", "MERGE_HEAD"}

type Manager struct {
	git      Git
	log      *slog.Logger
	totals   Totals
	pub      emit.Transport
	onActive func(path string, active bool)
	workers  chan struct{}
	linger   time.Duration

	mu     sync.Mutex
	rooms  map[string]*room
	leases map[device.ID]time.Time
	// sets is each device's last watch set, so only a change is logged.
	sets map[device.ID][]string
	// active is the paths a device has in front of the user, a subset of its
	// watch set: path → devices.
	active map[string]map[device.ID]struct{}
	// announcing is taken before mu is let go, so onActive hears opens and
	// closes in the order they happened, even from two devices at once.
	announcing sync.Mutex
}

type room struct {
	subs   map[device.ID]struct{}
	cancel context.CancelFunc
	// wake asks the watcher to look again: a file moved, or a device joined
	// while the room was idle.
	wake chan struct{}
	// idle ends the room when it fires; nil while any device holds it.
	idle *time.Timer

	// sent is the last git.changed payload, kept so an unchanged project costs
	// nothing on the wire: an idle repository pushes zero frames no matter how
	// often it is checked.
	sent []byte
}

type Deps struct {
	Git Git
	// Totals keeps each project's diff totals for the project list.
	Totals Totals
	// Pushes carries fs.change and git.changed to the devices watching.
	Pushes emit.Transport
	// Active is told when a path becomes active on its first device and when
	// its last lets go, lease expiry included: for work that runs only while a
	// project is in front of the user.
	Active func(path string, active bool)
	Log    *slog.Logger
}

func New(d Deps) *Manager {
	return &Manager{
		git:      d.Git,
		totals:   d.Totals,
		pub:      d.Pushes,
		onActive: d.Active,
		workers:  make(chan struct{}, 4),
		linger:   linger,
		log:      d.Log,
		rooms:    map[string]*room{},
		leases:   map[device.ID]time.Time{},
		sets:     map[device.ID][]string{},
		active:   map[string]map[device.ID]struct{}{},
	}
}

// Totals keeps each project's diff totals, persisted before a change is
// announced so a list read after the push agrees with it.
type Totals interface {
	UpdateProjectDiff(path string, available bool, files, additions, deletions int) (bool, error)
}

// Watch replaces a device's watch set and renews its lease. active is the
// part of paths the user has open. resync resends every watched path's last
// git.changed, for a device that relaunched inside its lease and holds nothing.
func (m *Manager) Watch(caller device.ID, paths, active []string, resync bool) error {
	if len(paths) > MaxPaths {
		return fmt.Errorf("%w: %d, max %d", ErrTooManyPaths, len(paths), MaxPaths)
	}
	// A path failing containment is dropped, not fatal: one deleted folder must
	// not silence the other forty-nine. Kept as the device spelled it, since the
	// phone routes a push by its own cwd.
	want := make(map[string]bool, len(paths))
	for _, path := range paths {
		if _, err := m.git.Contain(path); err == nil && path != "" {
			want[path] = true
		}
	}

	m.mu.Lock()
	m.leases[caller] = time.Now().Add(lease)

	// Leave the unwanted, then join the new, so a one-row change does not restart
	// the other watchers.
	for path, r := range m.rooms {
		if !want[path] {
			m.leaveLocked(path, r, caller)
		}
	}
	var greet [][]byte
	for path := range want {
		last := m.joinLocked(path, caller)
		if resync {
			last = m.rooms[path].sent
		}
		if last != nil {
			greet = append(greet, last)
		}
	}
	wantActive := map[string]bool{}
	for _, path := range active {
		if want[path] {
			wantActive[path] = true
		}
	}
	opened, closed := m.setActiveLocked(caller, wantActive)
	set := sortedKeys(want)
	changed := !slices.Equal(m.sets[caller], set)
	m.sets[caller] = set
	m.announcing.Lock()
	m.mu.Unlock()
	m.announce(closed, false)
	m.announce(opened, true)
	m.announcing.Unlock()
	if changed {
		m.log.Info("watch: device set", "device", caller, "paths", set, "active", sortedKeys(wantActive))
	} else {
		m.log.Debug("watch: renewed", "device", caller, "paths", len(set))
	}

	// A room already watched stays silent until something moves, so a device
	// joining it gets the current state now.
	for _, payload := range greet {
		if err := m.pub.Send(caller, Changed{}.Method(), payload); err != nil {
			m.log.Debug("watch: push failed", "device", caller, "err", err)
		}
	}
	return nil
}

// Stop drops a device from every room it is in.
func (m *Manager) Stop(caller device.ID) {
	m.mu.Lock()
	n := m.dropLocked(caller)
	_, closed := m.setActiveLocked(caller, nil)
	m.announcing.Lock()
	m.mu.Unlock()
	m.announce(closed, false)
	m.announcing.Unlock()
	if n > 0 {
		m.log.Info("watch: device stopped", "device", caller, "paths", n)
	}
}

// dropLocked forgets a device's lease and set and leaves its rooms, returning
// how many it was in.
func (m *Manager) dropLocked(caller device.ID) int {
	n := len(m.sets[caller])
	delete(m.leases, caller)
	delete(m.sets, caller)
	for path, r := range m.rooms {
		m.leaveLocked(path, r, caller)
	}
	return n
}

// setActiveLocked replaces a device's active paths and returns the paths that
// gained their first device and the ones that lost their last.
func (m *Manager) setActiveLocked(caller device.ID, want map[string]bool) (opened, closed []string) {
	for path, devices := range m.active {
		if _, ok := devices[caller]; ok && !want[path] {
			delete(devices, caller)
			if len(devices) == 0 {
				delete(m.active, path)
				closed = append(closed, path)
			}
		}
	}
	for path := range want {
		devices := m.active[path]
		if devices == nil {
			devices = map[device.ID]struct{}{}
			m.active[path] = devices
		}
		if _, ok := devices[caller]; !ok {
			devices[caller] = struct{}{}
			if len(devices) == 1 {
				opened = append(opened, path)
			}
		}
	}
	return opened, closed
}

// announce runs outside mu, under announcing: the listener may take its time
// without holding up pushes, but not reorder a close and an open.
func (m *Manager) announce(paths []string, active bool) {
	for _, path := range paths {
		m.onActive(path, active)
	}
}

// Run sweeps expired leases until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(lease / 4)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sweep()
		}
	}
}

func (m *Manager) sweep() {
	now := time.Now()
	m.mu.Lock()
	var expired []device.ID
	for id, deadline := range m.leases {
		if now.After(deadline) {
			expired = append(expired, id)
		}
	}
	var closed []string
	for _, id := range expired {
		m.dropLocked(id)
		_, gone := m.setActiveLocked(id, nil)
		closed = append(closed, gone...)
	}
	m.announcing.Lock()
	m.mu.Unlock()
	m.announce(closed, false)
	m.announcing.Unlock()

	for _, id := range expired {
		m.log.Info("watch: lease expired", "device", id)
	}
}

// joinLocked adds a device to a room, starting the watcher if it is the first.
// It returns the room's last push when the device is new to a running room.
func (m *Manager) joinLocked(path string, caller device.ID) []byte {
	if r, ok := m.rooms[path]; ok {
		if _, joined := r.subs[caller]; joined {
			return nil
		}
		r.subs[caller] = struct{}{}
		if r.idle != nil {
			// Back inside the linger: nothing was polled meanwhile, so look now.
			r.idle.Stop()
			r.idle = nil
			nudge(r.wake)
			m.log.Info("watch: watcher resumed", "path", path)
		}
		return r.sent
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &room{
		subs:   map[device.ID]struct{}{caller: {}},
		cancel: cancel,
		wake:   make(chan struct{}, 1),
	}
	m.rooms[path] = r
	go m.watch(ctx, path, r.wake)
	m.log.Info("watch: watcher started", "path", path)
	return nil
}

// leaveLocked removes a device; the last one out starts the room's linger.
func (m *Manager) leaveLocked(path string, r *room, caller device.ID) {
	if _, ok := r.subs[caller]; !ok {
		return
	}
	delete(r.subs, caller)
	if len(r.subs) > 0 || r.idle != nil {
		return
	}
	r.idle = time.AfterFunc(m.linger, func() { m.expire(path, r) })
	m.log.Info("watch: watcher idle", "path", path, "stops_in", m.linger)
}

// expire stops a room its linger outlived, unless a device came back.
func (m *Manager) expire(path string, r *room) {
	m.mu.Lock()
	if m.rooms[path] != r || len(r.subs) > 0 {
		m.mu.Unlock()
		return
	}
	r.cancel()
	delete(m.rooms, path)
	m.mu.Unlock()
	m.log.Info("watch: watcher stopped", "path", path)
}

// watch is the one goroutine per project.
func (m *Manager) watch(ctx context.Context, path string, wake chan struct{}) {
	var moved changedFiles
	stop, live := watchTree(path, func(file string, created bool) {
		moved.add(file, created)
		nudge(wake)
	})
	every := tick
	if live {
		defer stop()
		every = backstop
	}
	t := time.NewTicker(every)
	defer t.Stop()

	m.poll(ctx, path)
	var mark string
	var lastFull time.Time
	var settled <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			if settled == nil {
				settled = time.After(settle)
			}
		case <-settled:
			settled = nil
			// An idle room keeps what moved for a device that comes back.
			if len(m.subscribers(path)) == 0 {
				continue
			}
			m.poll(ctx, path)
			files, truncated := moved.take()
			files = m.dropIgnored(ctx, path, &moved, files)
			changes, truncated := capped(classify(path, files), truncated)
			m.sendChanges(path, changes, truncated)
		case now := <-t.C:
			next := fingerprint(path)
			// Without a tree watcher, the fingerprint catches what git writes (a
			// commit, a checkout, a stage) within one tick; the timer catches edits.
			if !live && next == mark && now.Sub(lastFull) < full {
				continue
			}
			mark, lastFull = next, now
			m.poll(ctx, path)
		}
	}
}

// nudge wakes a watcher without blocking; one pending wake covers many.
func nudge(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// subscribers is who a room pushes to now; nil while it lingers.
func (m *Manager) subscribers(path string) []device.ID {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[path]
	if !ok {
		return nil
	}
	ids := make([]device.ID, 0, len(r.subs))
	for id := range r.subs {
		ids = append(ids, id)
	}
	return ids
}

// poll computes the status and broadcasts it if it changed. A lingering room
// runs no git: nobody would hear it, and a device coming back wakes a poll.
func (m *Manager) poll(ctx context.Context, path string) {
	if len(m.subscribers(path)) == 0 {
		return
	}
	select {
	case m.workers <- struct{}{}:
		defer func() { <-m.workers }()
	case <-ctx.Done():
		return
	}

	statuses, err := m.git.Status(ctx, []string{path})
	if err != nil || len(statuses) == 0 {
		return
	}
	project := Project{Status: statuses[0]}
	project.Working = m.working(ctx, path)
	if ctx.Err() != nil {
		return
	}
	payload, err := json.Marshal(Changed{Projects: []Project{project}})
	if err != nil {
		return
	}

	m.mu.Lock()
	r, ok := m.rooms[path]
	if !ok || string(r.sent) == string(payload) {
		m.mu.Unlock()
		return
	}
	r.sent = payload
	watchers := make([]device.ID, 0, len(r.subs))
	for id := range r.subs {
		watchers = append(watchers, id)
	}
	m.mu.Unlock()

	// Marshalled once above; this loop is the entire fan-out.
	for _, id := range watchers {
		if err := m.pub.Send(id, Changed{}.Method(), payload); err != nil {
			m.log.Debug("watch: push failed", "device", id, "err", err)
		}
	}
}

// working reads the totals and persists them before they are announced, so a
// list read after the push agrees with it. nil for a plain folder.
func (m *Manager) working(ctx context.Context, path string) *git.ChangeSet {
	diff, err := m.git.WorkingDiff(ctx, path)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		diff = nil
	}
	var files, additions, deletions int
	if diff != nil {
		files, additions, deletions = diff.FilesChanged, diff.Additions, diff.Deletions
	}
	if _, err := m.totals.UpdateProjectDiff(path, diff != nil, files, additions, deletions); err != nil {
		m.log.Debug("watch: saving diff totals failed", "path", path, "err", err)
	}
	return diff
}

// fingerprint is a cheap summary of what git writes when HEAD, refs, or the
// index move. Not a hash of the worktree; `git status` answers that.
func fingerprint(path string) string {
	dir := git.Dir(path)
	var b []byte
	for _, name := range gitFiles {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			b = append(b, '-')
			continue
		}
		b = append(b, fmt.Sprintf("%d:%d;", info.ModTime().UnixNano(), info.Size())...)
	}
	return string(b)
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
