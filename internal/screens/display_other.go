//go:build !darwin && !windows

package screens

import "context"

// Elsewhere the host has no display of its own to stream.
const (
	hasOwnDisplay        = false
	displayNeedsPassword = true
)

func thisDisplay(context.Context) (Screen, bool) { return Screen{}, false }

// readyDisplay: there's no display here to get ready.
func readyDisplay(context.Context) error { return nil }
