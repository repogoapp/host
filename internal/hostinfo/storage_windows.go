package hostinfo

import "golang.org/x/sys/windows"

// volume is the size of the volume holding path and what this user may
// still write on it (quotas included), as Explorer shows it.
func volume(path string) (Storage, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return Storage{}, false
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil || total == 0 {
		return Storage{}, false
	}
	return Storage{TotalBytes: int64(total), FreeBytes: int64(free)}, true
}
