//go:build !(darwin && cgo)

package capture

import (
	"errors"
	"os"
)

// RunAudio needs macOS.
func RunAudio(*os.File, bool, int) error {
	return errors.New("sound from the screen needs macOS")
}
