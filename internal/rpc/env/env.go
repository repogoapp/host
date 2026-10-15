// Package env is the `env.*` methods: dotenv files bound to envFrom handles on
// this host, read for a device.
package env

import (
	"context"

	"github.com/repogoapp/host/internal/envsource"
	"github.com/repogoapp/host/internal/rpc"
)

type Deps struct {
	Sources *envsource.Sources
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

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "env.sources.list", d.list)
	rpc.Add(r, "env.sources.set", d.set)
	rpc.Add(r, "env.sources.remove", d.remove)
	rpc.Add(r, "env.read", d.read)
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
