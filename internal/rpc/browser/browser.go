// Package browser is the `browser.*` methods: a phone answering the preview
// actions RepoGo's MCP server sent it for a turn it started.
package browser

import (
	"context"

	"github.com/repogo/host/internal/repogomcp"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	Browser *repogomcp.Browser
}

type PendingResult struct {
	Requests []repogomcp.Request `json:"requests" wire:"array"`
}

type RespondParams struct {
	RequestID string           `json:"request_id"`
	Result    repogomcp.Result `json:"result"`
}

func Register(r *rpc.Router, d Deps) {
	// Both answer to the calling device only: a request goes to the phone
	// that sent the turn, and only that phone may answer it.
	rpc.Add(r, "browser.pending", d.pending)
	rpc.Add(r, "browser.respond", d.respond)
}

func (d Deps) pending(_ context.Context, c rpc.Caller, _ rpc.None) (PendingResult, error) {
	return PendingResult{Requests: d.Browser.Pending(c.Device)}, nil
}

func (d Deps) respond(_ context.Context, c rpc.Caller, a RespondParams) (rpc.Ack, error) {
	return rpc.OK, d.Browser.Respond(c.Device, a.RequestID, a.Result)
}
