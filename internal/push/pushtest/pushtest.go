// Package pushtest is a relay stand-in recording the pushes a host asks for.
package pushtest

import (
	"context"

	"github.com/repogoapp/host/internal/pushgate"
)

// Sender records every push and answers each with Reply.
type Sender struct {
	Calls []pushgate.PushRequest
	Reply error
}

func (s *Sender) Push(_ context.Context, req pushgate.PushRequest) error {
	s.Calls = append(s.Calls, req)
	return s.Reply
}
