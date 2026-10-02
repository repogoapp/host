// Package rpc is the one router every transport dispatches into, so a method
// answers the same over loopback, WebSocket, and the relay.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime/debug"
	"sort"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/wirejson"
)

// Scope records which proof the transport accepted: a device signature
// (remote) or the 0600 token only a same-machine process can read (local).
type Scope int

const (
	// ScopeRemote is a device that signed in, over the relay or the loopback socket.
	ScopeRemote Scope = iota

	// ScopeLocal is a caller that read the token out of the 0600 runtime file,
	// which only this user's own processes can do.
	ScopeLocal
)

// Caller is the authenticated identity behind one call. Always established by
// the transport, never read out of a payload.
type Caller struct {
	Device device.ID
	Scope  Scope
}

// The router's errors are errkind's roots, so a core package's own sentinel
// matches them by kind: errors.Is(files.ErrNotFound, rpc.ErrNotFound).
var (
	ErrUnsupported   = errors.New("unsupported method")
	ErrInvalidParams = errkind.ErrInvalid
	ErrNotFound      = errkind.ErrNotFound
	ErrDenied        = errkind.ErrDenied
	ErrUnavailable   = errkind.ErrUnavailable

	ErrLocalOnly  = errkind.New(errkind.Denied, "this method can only be called from the host machine")
	ErrPairedOnly = errkind.New(errkind.Denied, "this method can only be called from a paired device")
	ErrBusy       = errkind.New(errkind.Unavailable, "too many calls in flight; retry")
)

// Ack is the answer of every method that has nothing else to say.
type Ack struct {
	OK bool `json:"ok"`
}

var OK = Ack{OK: true}

// None is the params of a method that takes none.
type None struct{}

// Access is who may reach a method, declared where it is registered.
type Access uint8

const (
	Anyone Access = iota

	// LocalOnly: the caller must be at the machine, however well it authenticated.
	LocalOnly

	// PairedOnly: the method acts for the phone asking, so the machine has no say.
	PairedOnly
)

// Spec is one method as tooling sees it: the docs and the conformance suite
// are generated from these, never from a second list.
type Spec struct {
	Name   string
	Access Access

	// Detached runs on past its caller's disconnect: a push, clone or install
	// cut off halfway leaves worse than one that finishes unobserved.
	Detached bool

	// Unpaired lets a device the host has not paired call it; the pairing code guards it instead.
	Unpaired bool

	Params, Result reflect.Type
}

type Option func(*Spec)

var (
	Local    Option = func(s *Spec) { s.Access = LocalOnly }
	Paired   Option = func(s *Spec) { s.Access = PairedOnly }
	Detached Option = func(s *Spec) { s.Detached = true }
	Unpaired Option = func(s *Spec) { s.Unpaired = true }
)

type handler func(context.Context, Caller, json.RawMessage) (any, error)

type method struct {
	Spec
	fn handler
}

type Router struct {
	methods map[string]method
	log     *slog.Logger
}

func New(log *slog.Logger) *Router { return &Router{methods: map[string]method{}, log: log} }

// Add registers name, `family.method`, with its params and result read off
// fn's signature. A duplicate is a wiring bug and panics.
func Add[P, R any](r *Router, name string, fn func(context.Context, Caller, P) (R, error), opts ...Option) {
	if _, exists := r.methods[name]; exists {
		panic("rpc: duplicate method " + name)
	}
	spec := Spec{Name: name, Params: reflect.TypeFor[P](), Result: reflect.TypeFor[R]()}
	for _, opt := range opts {
		opt(&spec)
	}
	r.methods[name] = method{Spec: spec, fn: func(ctx context.Context, c Caller, raw json.RawMessage) (any, error) {
		var p P
		if err := jsonrpc.Into(raw, &p); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidParams, err)
		}
		return fn(ctx, c, p)
	}}
}

// Specs is the whole vocabulary, sorted by name.
func (r *Router) Specs() []Spec {
	out := make([]Spec, 0, len(r.methods))
	for _, m := range r.methods {
		out = append(out, m.Spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names is Specs' names; the conformance suite walks it.
func (r *Router) Names() []string {
	out := make([]string, 0, len(r.methods))
	for _, s := range r.Specs() {
		out = append(out, s.Name)
	}
	return out
}

// Unpaired reports whether a device the host has not paired may call name.
func (r *Router) Unpaired(name string) bool { return r.methods[name].Unpaired }

// Call dispatches one method. ctx ends when the caller goes away, except for a
// Detached method.
func (r *Router) Call(ctx context.Context, c Caller, name string, params json.RawMessage) (_ json.RawMessage, err error) {
	m, ok := r.methods[name]
	if !ok {
		return nil, fmt.Errorf("%w %s", ErrUnsupported, name)
	}
	switch {
	case m.Access == LocalOnly && c.Scope != ScopeLocal:
		return nil, fmt.Errorf("%w: %s", ErrLocalOnly, name)
	case m.Access == PairedOnly && c.Scope != ScopeRemote:
		return nil, fmt.Errorf("%w: %s", ErrPairedOnly, name)
	}
	if m.Detached {
		ctx = context.WithoutCancel(ctx)
	}
	// One handler's bug answers its caller; it must not take the host down.
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("rpc: handler panicked", "method", name, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("%s failed: %v", name, p)
		}
	}()
	out, err := m.fn(ctx, c, params)
	if err != nil {
		return nil, err
	}
	b, err := wirejson.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode result of %s: %w", name, err)
	}
	return b, nil
}

// Dispatch answers one inbound message; a notification gets no reply, not even
// an error.
func (r *Router) Dispatch(ctx context.Context, c Caller, m *jsonrpc.Message) *jsonrpc.Message {
	if !m.IsRequest() {
		return nil
	}
	out, err := r.Call(ctx, c, m.Method, m.Params)
	if err != nil {
		return jsonrpc.Fail(m.ID, Code(err), err.Error())
	}
	return &jsonrpc.Message{JSONRPC: jsonrpc.Version, ID: m.ID, Result: out}
}

// Code maps a handler error onto the wire, once, for every transport.
func Code(err error) int {
	if errors.Is(err, ErrUnsupported) {
		return jsonrpc.CodeMethodNotFound
	}
	switch errkind.Of(err) {
	case errkind.Invalid:
		return jsonrpc.CodeInvalidParams
	case errkind.NotFound:
		return jsonrpc.CodeNotFound
	case errkind.Denied:
		return jsonrpc.CodeDenied
	case errkind.Unavailable:
		return jsonrpc.CodeUnavailable
	}
	return jsonrpc.CodeInternal
}

// MaxInflight bounds one connection's concurrent calls: enough that a slow
// call never holds up the next, few enough that a flood cannot exhaust the host.
const MaxInflight = 32

// Inflight is one connection's call slots. Every transport serves calls the
// same way: each on its own goroutine, refused as busy past the bound.
type Inflight chan struct{}

func NewInflight() Inflight { return make(Inflight, MaxInflight) }

// Go runs fn on its own goroutine, or reports false when every slot is taken.
func (f Inflight) Go(fn func()) bool {
	select {
	case f <- struct{}{}:
	default:
		return false
	}
	go func() {
		defer func() { <-f }()
		fn()
	}()
	return true
}

// Busy is the reply to a request refused for want of a slot; nil for a
// notification, which gets no reply.
func Busy(m *jsonrpc.Message) *jsonrpc.Message {
	if !m.IsRequest() {
		return nil
	}
	return jsonrpc.Fail(m.ID, Code(ErrBusy), ErrBusy.Error())
}

// Encode is a reply's bytes. One that cannot be encoded, too large most often,
// becomes an error reply: silence would cost the caller its whole timeout.
func (r *Router) Encode(reply *jsonrpc.Message) ([]byte, error) {
	b, err := jsonrpc.Encode(reply)
	if err != nil {
		r.log.Error("rpc: encode reply", "err", err)
		return jsonrpc.Encode(jsonrpc.Fail(reply.ID, jsonrpc.CodeInternal, err.Error()))
	}
	return b, nil
}
