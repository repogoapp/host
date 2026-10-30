package files

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// MountedDrives are the drives mounted in /Volumes that Finder shows, from the
// mount table: MNT_NOWAIT answers without asking any drive, so a dropped share
// cannot hold it. The startup disk mounts at /, so it is not one.
func MountedDrives() []string {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || n == 0 {
		return nil
	}
	mounts := make([]unix.Statfs_t, n)
	if n, err = unix.Getfsstat(mounts, unix.MNT_NOWAIT); err != nil {
		return nil
	}
	out := []string{}
	for _, mount := range mounts[:n] {
		path := unix.ByteSliceToString(mount.Mntonname[:])
		if filepath.Dir(path) == "/Volumes" && mount.Flags&unix.MNT_DONTBROWSE == 0 {
			out = append(out, path)
		}
	}
	return out
}
