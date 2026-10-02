// Package env is the `env.*` methods: dotenv files bound to envFrom handles on
// this host, read for a device, and the secrets requests of this host's own
// starts, answered by a device.
package env

import (
	"context"

	"github.com/repogo/host/internal/envsource"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	Sources  *envsource.Sources
	Requests *envsource.Requests
}

type HandleParams struct {
	Handle string `json:"handle"`
}

type SetParams struct {
	HandleParams
	Path string `json:"path"`
}

type ListResult struct {
	Sources []envsource.Source `json:"sources" wire:"array"`
}

type SetResult struct {
	Source envsource.Source `json:"source"`
}

type ReadParams struct {
	Handles []string `json:"handles"`
}

type ReadResponse struct {
	Results map[string]envsource.ReadResult `json:"results"`
}

type PendingResult struct {
	Requests []envsource.Request `json:"requests" wire:"array"`
}

type ProvideParams struct {
	RequestID string                          `json:"request_id"`
	Approved  bool                            `json:"approved"`
	Results   map[string]envsource.ReadResult `json:"results"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "env.sources.list", d.list)
	rpc.Add(r, "env.sources.set", d.set)
	rpc.Add(r, "env.sources.remove", d.remove)
	rpc.Add(r, "env.read", d.read)
	rpc.Add(r, "env.pending", d.pending)
	rpc.Add(r, "env.provide", d.provide)
}

func (d Deps) list(context.Context, rpc.Caller, rpc.None) (ListResult, error) {
	return ListResult{Sources: d.Sources.List()}, nil
}

func (d Deps) set(_ context.Context, _ rpc.Caller, a SetParams) (SetResult, error) {
	src, err := d.Sources.Set(a.Handle, a.Path)
	return SetResult{Source: src}, err
}

func (d Deps) remove(_ context.Context, _ rpc.Caller, a HandleParams) (rpc.Ack, error) {
	return rpc.OK, d.Sources.Remove(a.Handle)
}

func (d Deps) read(_ context.Context, _ rpc.Caller, a ReadParams) (ReadResponse, error) {
	return ReadResponse{Results: d.Sources.Read(a.Handles)}, nil
}

func (d Deps) pending(context.Context, rpc.Caller, rpc.None) (PendingResult, error) {
	return PendingResult{Requests: d.Requests.Pending()}, nil
}

func (d Deps) provide(_ context.Context, _ rpc.Caller, a ProvideParams) (rpc.Ack, error) {
	return rpc.OK, d.Requests.Provide(a.RequestID, a.Approved, a.Results)
}
