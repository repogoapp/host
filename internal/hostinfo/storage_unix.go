//go:build unix

package hostinfo

import "golang.org/x/sys/unix"

// volume is the size of the volume holding path and what an unprivileged
// user may still write on it.
func volume(path string) (Storage, bool) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil || fs.Blocks == 0 {
		return Storage{}, false
	}
	block := int64(fs.Bsize)
	return Storage{TotalBytes: int64(fs.Blocks) * block, FreeBytes: int64(fs.Bavail) * block}, true
}
