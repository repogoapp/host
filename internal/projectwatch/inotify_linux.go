package projectwatch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// watchMask is what moves in a folder that a diff or an editor would show.
const watchMask = unix.IN_CREATE | unix.IN_MOVED_TO | unix.IN_MOVED_FROM | unix.IN_DELETE |
	unix.IN_MODIFY | unix.IN_ATTRIB | unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW

// structural is a folder appearing, leaving or being renamed; the rest of a
// folder's own events (its mode, its times) change nothing a device shows.
const structural = unix.IN_CREATE | unix.IN_MOVED_TO | unix.IN_MOVED_FROM | unix.IN_DELETE

// inotify watches a tree one folder at a time, which is all the kernel
// offers. Once started, only its reader touches dirs.
type inotify struct {
	// file reads the descriptor; fd names it to the kernel, since asking the
	// file for it would make it blocking and Close could not end a Read.
	file   *os.File
	fd     int
	paths  []string
	keep   func(dir string) bool
	onPath func(string, bool)
	onLost func()
	// dirs is each watch's folder, by the descriptor events name it with.
	dirs map[int32]string
}

// watchEvents watches paths and every folder under them that keep accepts.
// False when the kernel has no watches left for the whole tree: half a tree
// watched would read as no changes in the other half, so the caller polls.
func watchEvents(paths []string, keep func(string) bool, onPath func(string, bool), onLost func()) (func(), bool) {
	w, ok := openInotify(paths, keep, onPath, onLost)
	if !ok {
		return nil, false
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.read()
	}()
	return func() {
		w.file.Close()
		<-done
	}, true
}

// openInotify watches the tree as it stands, reading nothing yet.
func openInotify(paths []string, keep func(string) bool, onPath func(string, bool), onLost func()) (*inotify, bool) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, false
	}
	w := &inotify{
		// Non-blocking, so the runtime polls it and Close ends the reader's Read.
		file: os.NewFile(uintptr(fd), "inotify"), fd: fd,
		paths: paths, keep: keep, onPath: onPath, onLost: onLost,
		dirs: map[int32]string{},
	}
	for _, path := range paths {
		if err := w.addTree(path, nil); err != nil {
			w.file.Close()
			return nil, false
		}
	}
	return w, true
}

// addTree watches top and the kept folders under it, telling found each file
// already there. Each folder is watched before it is listed, so a file landing
// meanwhile is reported once or twice, never missed. The only error is the
// kernel's watch limit; a folder that vanished or cannot be read is skipped.
func (w *inotify) addTree(top string, found func(file string)) error {
	return filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if found != nil {
				found(path)
			}
			return nil
		}
		if path != top && !w.keep(path) {
			return fs.SkipDir
		}
		wd, err := unix.InotifyAddWatch(w.fd, path, watchMask)
		if errors.Is(err, unix.ENOSPC) {
			return err
		}
		if err != nil {
			return fs.SkipDir
		}
		w.dirs[int32(wd)] = path
		return nil
	})
}

// forget drops the watches at and under a folder that was renamed away: the
// kernel keeps them on the moved folder, under a path that is no longer true.
func (w *inotify) forget(dir string) {
	for wd, path := range w.dirs {
		if path == dir || strings.HasPrefix(path, dir+"/") {
			_, _ = unix.InotifyRmWatch(w.fd, uint32(wd))
			delete(w.dirs, wd)
		}
	}
}

// read handles events until the descriptor is closed.
func (w *inotify) read() {
	buf := make([]byte, 64<<10)
	for {
		n, err := w.file.Read(buf)
		if err != nil {
			return
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			wd, mask, size := ev.Wd, ev.Mask, int(ev.Len)
			off += unix.SizeofInotifyEvent
			// The name is padded with NULs to align the next event.
			name := strings.TrimRight(string(buf[off:off+size]), "\x00")
			off += size
			w.handle(wd, mask, name)
		}
	}
}

func (w *inotify) handle(wd int32, mask uint32, name string) {
	if mask&unix.IN_Q_OVERFLOW != 0 {
		// Folders made while events were dropped have no watch yet.
		for _, path := range w.paths {
			_ = w.addTree(path, nil)
		}
		w.onLost()
		return
	}
	dir, ok := w.dirs[wd]
	if !ok {
		return
	}
	if mask&unix.IN_IGNORED != 0 {
		delete(w.dirs, wd)
		return
	}
	if name == "" {
		return
	}
	path := filepath.Join(dir, name)
	made := mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0
	if mask&unix.IN_ISDIR != 0 {
		switch {
		case mask&structural == 0:
			return
		case made && w.keep(path):
			// Files can land in a new folder before its watch does.
			_ = w.addTree(path, func(file string) { w.onPath(file, true) })
		case mask&unix.IN_MOVED_FROM != 0:
			w.forget(path)
		}
	}
	w.onPath(path, made)
}
