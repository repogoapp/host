package projectwatch

import (
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// readBuffer is how much one read of a tree's changes holds; a burst past it
// is reported lost (an empty read), and the caller polls to catch up.
const readBuffer = 64 << 10

// changeFilter is what counts as a change: a file or folder made, removed or
// renamed, and a file written.
const changeFilter = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
	windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_SIZE | windows.FILE_NOTIFY_CHANGE_CREATION

// watchEvents streams every change under paths through ReadDirectoryChangesW,
// one handle per tree, as FSEvents does on macOS: so it asks nothing about
// which folders are watched. A burst too big for the buffer is reported lost.
func watchEvents(paths []string, _ func(string) bool, onPath func(string, bool), onLost func()) (func(), bool) {
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, false
	}
	var handles []windows.Handle
	closeAll := func() {
		for _, h := range handles {
			_ = windows.CloseHandle(h)
		}
		_ = windows.CloseHandle(stop)
	}
	for _, path := range paths {
		h, err := openTree(path)
		if err != nil {
			closeAll()
			return nil, false
		}
		handles = append(handles, h)
	}
	var wg sync.WaitGroup
	for i, h := range handles {
		wg.Add(1)
		go func(root string, h windows.Handle) {
			defer wg.Done()
			readChanges(root, h, stop, onPath, onLost)
		}(paths[i], h)
	}
	return func() {
		_ = windows.SetEvent(stop)
		wg.Wait()
		closeAll()
	}, true
}

func openTree(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
}

// readChanges reads root's changes until stop is set, each read overlapped
// so a stop can cancel the one waiting.
func readChanges(root string, h, stop windows.Handle, onPath func(string, bool), onLost func()) {
	done, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return
	}
	defer windows.CloseHandle(done)
	buf := make([]byte, readBuffer)
	for {
		ov := windows.Overlapped{HEvent: done}
		_ = windows.ResetEvent(done)
		if err := windows.ReadDirectoryChanges(h, &buf[0], uint32(len(buf)), true, changeFilter, nil, &ov, 0); err != nil &&
			err != windows.ERROR_IO_PENDING {
			return
		}
		which, err := windows.WaitForMultipleObjects([]windows.Handle{done, stop}, false, windows.INFINITE)
		if err != nil || which == windows.WAIT_OBJECT_0+1 {
			var n uint32
			_ = windows.CancelIoEx(h, &ov)
			_ = windows.GetOverlappedResult(h, &ov, &n, true)
			return
		}
		var n uint32
		if err := windows.GetOverlappedResult(h, &ov, &n, false); err != nil {
			if err == windows.ERROR_NOTIFY_ENUM_DIR {
				onLost()
				continue
			}
			return
		}
		if n == 0 {
			onLost()
			continue
		}
		for _, c := range parseChanges(buf[:n]) {
			onPath(filepath.Join(root, c.name), c.created)
		}
	}
}

type change struct {
	name    string
	created bool
}

// parseChanges reads a buffer of FILE_NOTIFY_INFORMATION records: each a
// name relative to the watched folder, and what happened to it.
func parseChanges(buf []byte) []change {
	var out []change
	for offset := 0; offset+12 <= len(buf); {
		info := (*windows.FileNotifyInformation)(unsafe.Pointer(&buf[offset]))
		size := int(info.FileNameLength)
		if offset+12+size > len(buf) {
			break
		}
		name := windows.UTF16ToString(unsafe.Slice(&info.FileName, size/2))
		created := info.Action == windows.FILE_ACTION_ADDED || info.Action == windows.FILE_ACTION_RENAMED_NEW_NAME
		out = append(out, change{name: name, created: created})
		if info.NextEntryOffset == 0 {
			break
		}
		offset += int(info.NextEntryOffset)
	}
	return out
}
