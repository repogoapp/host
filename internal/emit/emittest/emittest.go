// Package emittest is a Sink stand-in for a producer's tests.
package emittest

import (
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
)

// Sink hands each event to the function a test set; an unset one drops it.
type Sink struct {
	OnAll func(emit.Event)
	OnTo  func(device.ID, emit.Event) error
}

func (s Sink) All(ev emit.Event) {
	if s.OnAll != nil {
		s.OnAll(ev)
	}
}

func (s Sink) To(d device.ID, ev emit.Event) error {
	if s.OnTo != nil {
		return s.OnTo(d, ev)
	}
	return nil
}
