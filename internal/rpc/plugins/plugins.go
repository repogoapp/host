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
	// Confirmed is the user's yes to the method's confirm question; without
	// it, a method that has one answers CONFIRM instead of running.
	Confirmed bool `json:"confirmed,omitempty"`
}

// SubscribeParams names a query to keep running, as `<plugin>.<method>`, under
// the caller's own id; subscribing again under an id replaces it.
type SubscribeParams struct {
	Project      string          `json:"project"`
	Method       string          `json:"method"`
	Input        json.RawMessage `json:"input,omitempty"`
	Subscription string          `json:"subscription"`
}

type UnsubscribeParams struct {
	Subscription string `json:"subscription"`
}

// LogsParams narrows the console lines plugins wrote; every field may be left out.
type LogsParams struct {
	Plugin string `json:"plugin" doc:"Only this plugin's lines; empty is every plugin"`
	Level  string `json:"level" doc:"This level and worse: debug, info, warn or error; empty is every level"`
	Method string `json:"method" doc:"Only lines written while this method ran, as <plugin>.<method>"`
	Since  int64  `json:"since" doc:"Only lines at or after this time, in ms since the epoch"`
	Until  int64  `json:"until" doc:"Only lines before this time, in ms since the epoch"`
	Query  string `json:"query" doc:"Only lines whose text contains this"`
	Limit  int    `json:"limit" doc:"At most this many, newest first; 100 unless said, at most 1000"`
}

type LogsResult struct {
	Lines []pluginscore.LogLine `json:"lines"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "plugins.logs", d.logs, rpc.Tool("The console lines plugins wrote, newest first, each with the plugin, "+
		"the method and the call that wrote it, and errors methods threw. Read it when a plugin or a card showing one misbehaves."),
		rpc.Read, rpc.LiveOn("plugins.logged"))
	rpc.Add(r, "plugins.call", d.call)
	rpc.Add(r, "plugins.subscribe", d.subscribe)
	rpc.Add(r, "plugins.unsubscribe", d.unsubscribe)
}

func (d Deps) logs(ctx context.Context, _ rpc.Caller, p LogsParams) (LogsResult, error) {
	lines, err := d.Plugins.Logs(ctx, pluginscore.LogQuery{
		Plugin: p.Plugin, Level: p.Level, Method: p.Method, Since: p.Since, Until: p.Until, Query: p.Query, Limit: p.Limit,
	})
	return LogsResult{Lines: lines}, err
}

func (d Deps) call(ctx context.Context, _ rpc.Caller, p CallParams) (pluginscore.Answer, error) {
	if p.Method == "" {
		return pluginscore.Answer{}, rpc.ErrInvalidParams
	}
	return d.Plugins.Call(ctx, p.Project, p.Method, p.Input, p.Confirmed, pluginscore.Origin{Caller: pluginscore.CallerCard})
}

// subscribe runs a query now and again whenever a write changes its answer,
// pushing plugins.data to the calling device until it unsubscribes or leaves.
func (d Deps) subscribe(ctx context.Context, c rpc.Caller, p SubscribeParams) (pluginscore.Subscribed, error) {
	if p.Method == "" {
		return pluginscore.Subscribed{}, rpc.ErrInvalidParams
	}
	return d.Plugins.Subscribe(ctx, c.Device, p.Project, p.Method, p.Subscription, p.Input)
}

func (d Deps) unsubscribe(ctx context.Context, c rpc.Caller, p UnsubscribeParams) (rpc.Ack, error) {
	return rpc.OK, d.Plugins.Unsubscribe(ctx, c.Device, p.Subscription)
}
