//go:build !darwin

package files

// MountedDrives is empty off macOS: other systems mount drives where their
// owner chooses, so the folder picker lists none.
func MountedDrives() []string { return nil }
