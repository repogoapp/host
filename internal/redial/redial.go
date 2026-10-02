// Package redial is the one reconnect loop for a link that dials out: a
// jittered, doubling wait between attempts, capped, and reset after a session
// that lasted.
package redial

import (
	"context"
	"math/rand/v2"
	"time"
)

const (
	Min = 1 * time.Second
	Max = 30 * time.Second

	// healthy is how long a session must last for its end to count as a fresh problem.
	healthy = time.Minute
)

// Run calls session until ctx ends; retry hears each end and the wait before
// the next attempt. A link that gives up looks permanently offline.
func Run(ctx context.Context, session func(context.Context) error, retry func(err error, wait time.Duration)) {
	var b backoff
	for ctx.Err() == nil {
		start := time.Now()
		err := session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > healthy {
			b = backoff{}
		}
		wait := b.next()
		retry(err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// backoff doubles from Min to Max; the zero value starts over.
type backoff struct{ d time.Duration }

// next is the wait before the next attempt: somewhere in the upper half of the
// current step, so a relay restart is not answered by every host at once.
func (b *backoff) next() time.Duration {
	b.d = min(max(b.d*2, Min), Max)
	return b.d/2 + rand.N(b.d/2+1)
}
