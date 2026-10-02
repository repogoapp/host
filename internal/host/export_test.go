package host

import "context"

// Spawn runs fn as an owned worker, for a test that stands in for one.
func (h *Host) Spawn(fn func(context.Context)) { h.spawn(fn) }
