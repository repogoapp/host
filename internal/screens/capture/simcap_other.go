//go:build !(darwin && cgo)

package capture

import (
	"context"
	"errors"
	"io"
	"os"
)

func start(string, string, bool, bool, *os.File) error {
	return errors.New("simulator capture needs macOS")
}

func touch(int, float64, float64) {}
func button(int, int)             {}
func hid(int, int)                {}
func key(int, bool)               {}
func rotate(int) bool             { return false }
func memoryWarning()              {}
func onscreenKeyboard(bool) bool  { return false }
func fold(string) bool            { return false }
func tell([]byte)                 {}
func keyframe()                   {}
func turn(int)                    {}
func pace(Input)                  {}
func caDebug(string, bool)        {}
func stop()                       {}
func elements() []byte            { return nil }

func serviceButton(int, int, int) bool { return false }

// RunAndroid needs macOS, for its encoder.
func RunAndroid(context.Context, int, *os.File, io.Reader) error {
	return errors.New("emulator capture needs macOS")
}
