// Package hostupdate replaces the running host with the latest release and
// restarts into it, from the CLI or a phone, and rolls back a release that
// cannot start.
package hostupdate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/release"
)

var (
	ErrDevBuild = errkind.New(errkind.Unavailable, "a development build does not update itself")
	ErrRunning  = errkind.New(errkind.Unavailable, "an update is already running")
)

// Busy is the work an update would stop.
type Busy struct {
	Chats     int `json:"chats"`
	Terminals int `json:"terminals"`
	Actions   int `json:"actions"`
}

func (b Busy) any() bool { return b.Chats+b.Terminals+b.Actions > 0 }

// Result is one update request. To is empty when the host is already current;
// Busy is set, and nothing changed, when the update would stop work the caller
// has not agreed to stop. Otherwise the host restarts into To shortly after.
type Result struct {
	From string `json:"from"`
	To   string `json:"to,omitempty"`
	Busy *Busy  `json:"busy,omitempty"`
}

// Turns is the turn manager: closing admission, and ending what runs.
type Turns interface {
	GuardUpdate(enabled, force bool) error
	ActiveChats() int
	StopAll(ctx context.Context) error
}

type Deps struct {
	// Version is the running release. Root is what an update replaces: the
	// binary, or on macOS the app bundle it runs from (release.Root), found
	// at start because Linux names a replaced executable "(deleted)".
	Version string
	Root    string
	// Marker is the update.json that lets the next start settle or roll back.
	Marker string

	Latest func(context.Context) (release.Manifest, error)
	// Stage downloads and verifies the release beside root.
	Stage func(ctx context.Context, m release.Manifest, root string) (string, error)

	Turns     Turns
	Terminals func() int
	Actions   func() int

	// Restart shuts the host down and starts the new release in its place.
	Restart func()
}

type Updater struct {
	d  Deps
	mu sync.Mutex
}

func New(d Deps) *Updater { return &Updater{d: d} }

// restartDelay lets the reply reach the caller before the host goes away.
const restartDelay = time.Second

// Update installs the latest release. now agrees to stop running chats,
// terminals and actions; without it an update that would stop any is refused
// with the count, so the caller can ask.
func (u *Updater) Update(ctx context.Context, now bool) (Result, error) {
	if !u.mu.TryLock() {
		return Result{}, ErrRunning
	}
	restarting := false
	defer func() {
		if !restarting {
			u.mu.Unlock()
		}
	}()
	out := Result{From: u.d.Version}
	if u.d.Version == "dev" {
		return out, ErrDevBuild
	}
	latest, err := u.d.Latest(ctx)
	if err != nil {
		return out, err
	}
	if !release.Newer(latest.Version, u.d.Version) {
		return out, nil
	}
	out.To = latest.Version
	busy := Busy{Chats: u.d.Turns.ActiveChats(), Terminals: u.d.Terminals(), Actions: u.d.Actions()}
	if busy.any() && !now {
		out.Busy = &busy
		return out, nil
	}
	staged, err := u.d.Stage(ctx, latest, u.d.Root)
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(staged)
	if err := u.d.Turns.GuardUpdate(true, now); err != nil {
		return out, err
	}
	defer func() {
		if !restarting {
			u.d.Turns.GuardUpdate(false, false)
		}
	}()
	if now {
		stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := u.d.Turns.StopAll(stopCtx)
		cancel()
		if err != nil {
			return out, err
		}
	}
	if err := u.swap(staged, marker{From: u.d.Version, To: latest.Version}); err != nil {
		return out, err
	}
	restarting = true
	time.AfterFunc(restartDelay, u.d.Restart)
	return out, nil
}

// swap keeps the running release as .previous for a rollback, records the
// update, and moves the new one into place. The running process keeps the
// files it started from, whatever their name.
func (u *Updater) swap(staged string, m marker) error {
	root := u.d.Root
	previous := root + ".previous"
	if err := os.RemoveAll(previous); err != nil {
		return err
	}
	if err := os.Rename(root, previous); err != nil {
		return fmt.Errorf("keep the current host for a rollback: %w", err)
	}
	if err := writeMarker(u.d.Marker, m); err != nil {
		os.Rename(previous, root)
		return err
	}
	if err := os.Rename(staged, root); err != nil {
		os.Remove(u.d.Marker)
		os.Rename(previous, root)
		return err
	}
	return nil
}

// marker is update.json. Attempts counts starts of To that never settled.
type marker struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Attempts int    `json:"attempts,omitempty"`
	Failed   string `json:"failed,omitempty"`
}

// maxAttempts is how many times a new release may fail to start before the
// host goes back to the one it replaced.
const maxAttempts = 3

// Outcome is what Resume found at start.
type Outcome struct {
	// RolledBack means root is the previous release again and must be
	// started in this process's place.
	RolledBack bool
	// Failed says why the last update did not take, once the previous
	// release is running again.
	Failed string
}

// Resume runs at start, before anything else: it counts a start of a new
// release and rolls back one that keeps failing before Settle.
func Resume(path, root, version string) (Outcome, error) {
	m, err := readMarker(path)
	if err != nil || m == nil {
		return Outcome{}, err
	}
	switch version {
	case m.To:
		m.Attempts++
		if m.Attempts < maxAttempts {
			return Outcome{}, writeMarker(path, *m)
		}
		if err := rollBack(root); err != nil {
			return Outcome{}, fmt.Errorf("roll back %s: %w", m.To, err)
		}
		m.Failed = fmt.Sprintf("%s didn't start, so the host went back to %s.", m.To, m.From)
		return Outcome{RolledBack: true}, writeMarker(path, *m)
	case m.From:
		return Outcome{Failed: m.Failed}, removeMarker(path)
	}
	return Outcome{}, removeMarker(path)
}

// rollBack puts the .previous release back at root. A bundle is a folder,
// which a rename cannot replace, so the failed release goes first.
func rollBack(root string) error {
	if _, err := os.Stat(root + ".previous"); err != nil {
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	return os.Rename(root+".previous", root)
}

// Settle marks the running release as started, once it is serving.
func Settle(path, version string) error {
	m, err := readMarker(path)
	if err != nil || m == nil || m.To != version {
		return err
	}
	return removeMarker(path)
}

// Exec replaces this process with binary, keeping its pid, arguments and
// environment, so a terminal or a container with nothing to restart the host
// sees the same process carry on.
func Exec(binary string) error {
	return syscall.Exec(binary, os.Args, os.Environ())
}

func readMarker(path string) (*marker, error) {
	var m marker
	found, err := apphome.ReadJSON(path, &m)
	switch {
	case !found:
		return nil, err
	case err != nil:
		// Unreadable is no update in flight; a rollback needs a known From.
		return nil, removeMarker(path)
	}
	return &m, nil
}

func writeMarker(path string, m marker) error {
	return apphome.WriteJSON(path, m, 0o600)
}

func removeMarker(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
