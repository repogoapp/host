// Package pushtest is a relay stand-in recording the pushes a host asks for.
package pushtest

import (
	"context"
	"encoding/json"

	"github.com/repogo/host/internal/relay"
)

// Sender records every push.send and answers each with Reply.
type Sender struct {
	Calls []relay.PushRequest
	Reply error
}

func (s *Sender) Control(_ context.Context, method string, params any) (json.RawMessage, error) {
	if method != "push.send" {
		panic(method)
	}
	s.Calls = append(s.Calls, params.(relay.PushRequest))
	return nil, s.Reply
}
