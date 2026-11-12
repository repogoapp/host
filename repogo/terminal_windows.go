package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// isTerminal is whether f is a console a person types at; NUL and a pipe
// have no console mode, so they answer no.
func isTerminal(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}
