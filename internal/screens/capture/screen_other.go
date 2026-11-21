//go:build !(darwin && cgo) && !(windows && cgo)

package capture

import (
	"context"
	"errors"
	"io"
	"os"
)

// RunScreen needs macOS or Windows.
func RunScreen(context.Context, *os.File, io.Reader) error {
	return errors.New("screen capture needs macOS or Windows")
}
