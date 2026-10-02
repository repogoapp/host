package wsserver

import (
	"context"
	"log/slog"
	"testing"

	"github.com/repogo/host/internal/rpc"
)

// The HTTP guard waives the token for exactly the router's Unpaired methods.
func TestTokenExemptFollowsTheRouter(t *testing.T) {
	r := rpc.New(slog.New(slog.DiscardHandler))
	ack := func(context.Context, rpc.Caller, rpc.None) (rpc.Ack, error) { return rpc.OK, nil }
	rpc.Add(r, "pair.complete", ack, rpc.Unpaired)
	rpc.Add(r, "pair.begin", ack)
	s := &Server{cfg: Config{Router: r}}
	for path, want := range map[string]bool{
		"/v1/rpc/pair/complete": true,
		"/v1/rpc/pair/begin":    false,
		"/v1/rpc/pair":          false,
		"/pair/complete":        false,
	} {
		if got := s.tokenExempt(path); got != want {
			t.Errorf("tokenExempt(%q) = %v, want %v", path, got, want)
		}
	}
}
