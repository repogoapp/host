// Package plugins is the `plugins.*` methods: a device runs a project's plugin methods
// and subscribes to its queries; the plugins themselves run in internal/plugins.
package plugins

import (
	"context"
	"encoding/json"

	pluginscore "github.com/repogoapp/host/internal/plugins"
	"github.com/repogoapp/host/internal/rpc"
)

type Deps struct {
	Plugins *pluginscore.Service
}

// CallParams names a method of one of a project's plugins, as `<plugin>.<method>`.
type CallParams struct {
	// Project is the project's folder on this host.
	Project string          `json:"project"`
	Method  string          `json:"method"`
	Input   json.RawMessage `json:"input,omitempty"`
}

// SubscribeParams names a query to keep running, as `<plugin>.<method>`.
type SubscribeParams struct {
	Project string          `json:"project"`
	Method  string          `json:"method"`
	Input   json.RawMessage `json:"input,omitempty"`
}

type UnsubscribeParams struct {
	Subscription string `json:"subscription"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "plugins.call", d.call)
	rpc.Add(r, "plugins.subscribe", d.subscribe)
	rpc.Add(r, "plugins.unsubscribe", d.unsubscribe)
}

func (d Deps) call(ctx context.Context, _ rpc.Caller, p CallParams) (pluginscore.Answer, error) {
	if p.Method == "" {
		return pluginscore.Answer{}, rpc.ErrInvalidParams
	}
	return d.Plugins.Call(ctx, p.Project, p.Method, p.Input)
}

// subscribe runs a query now and again whenever a write changes its answer,
// pushing plugins.data to the calling device until it unsubscribes or leaves.
func (d Deps) subscribe(ctx context.Context, c rpc.Caller, p SubscribeParams) (pluginscore.Subscribed, error) {
	if p.Method == "" {
		return pluginscore.Subscribed{}, rpc.ErrInvalidParams
	}
	return d.Plugins.Subscribe(ctx, c.Device, p.Project, p.Method, p.Input)
}

func (d Deps) unsubscribe(ctx context.Context, c rpc.Caller, p UnsubscribeParams) (rpc.Ack, error) {
	return rpc.OK, d.Plugins.Unsubscribe(ctx, c.Device, p.Subscription)
}
