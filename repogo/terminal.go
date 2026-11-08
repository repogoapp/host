package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal is whether f is a terminal a person types at; /dev/null and a
// pipe are character devices or files too, but answer no here.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlReadTermios)
	return err == nil
}
