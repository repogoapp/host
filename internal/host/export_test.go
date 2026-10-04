package host

import "context"

// Spawn runs fn as an owned worker, for a test that stands in for one.
func (h *Host) Spawn(fn func(context.Context)) { h.spawn(fn) }

// WaitWithin is Close's bounded wait for its workers.
var WaitWithin = waitWithin
