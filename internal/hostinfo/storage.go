package hostinfo

import (
	"os"

	"golang.org/x/sys/unix"
)

// Storage is the size of the volume holding the home folder and the space
// left on it; the phone works out what is used.
type Storage struct {
	TotalBytes int64 `json:"total_bytes"`
	// FreeBytes is what an unprivileged user may still write, as df's Avail.
	FreeBytes int64 `json:"free_bytes"`
}

// homeStorage is false when the home volume can't be read.
func homeStorage() (Storage, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Storage{}, false
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(home, &fs); err != nil || fs.Blocks == 0 {
		return Storage{}, false
	}
	block := int64(fs.Bsize)
	return Storage{TotalBytes: int64(fs.Blocks) * block, FreeBytes: int64(fs.Bavail) * block}, true
}
