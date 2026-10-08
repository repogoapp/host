// Package services is the `services.*` methods: the starts of a project's
// environment.json services waiting for a device to approve them.
package services

import (
	"context"

	"github.com/repogo/host/internal/rpc"
	servicescore "github.com/repogo/host/internal/services"
)

type Deps struct {
	Services *servicescore.Manager
}

type PendingResult struct {
	Requests []servicescore.Request `json:"requests" wire:"array"`
}

type AnswerParams struct {
	RequestID string `json:"request_id"`
	Approved  bool   `json:"approved"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "services.pending", d.pending)
	rpc.Add(r, "services.answer", d.answer)
}

func (d Deps) pending(context.Context, rpc.Caller, rpc.None) (PendingResult, error) {
	return PendingResult{Requests: d.Services.Pending()}, nil
}

func (d Deps) answer(_ context.Context, _ rpc.Caller, a AnswerParams) (rpc.Ack, error) {
	return rpc.OK, d.Services.Answer(a.RequestID, a.Approved)
}
