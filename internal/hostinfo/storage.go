package hostinfo

import "os"

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
	return volume(home)
}
