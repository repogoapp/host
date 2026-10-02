package rpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/rpc"
)

type echo struct {
	Text string `json:"text"`
}

func router() *rpc.Router {
	r := rpc.New(slog.New(slog.DiscardHandler))
	rpc.Add(r, "t.echo", func(_ context.Context, _ rpc.Caller, a echo) (echo, error) { return a, nil })
	rpc.Add(r, "t.local", func(context.Context, rpc.Caller, rpc.None) (rpc.Ack, error) { return rpc.OK, nil }, rpc.Local)
	rpc.Add(r, "t.paired", func(context.Context, rpc.Caller, rpc.None) (rpc.Ack, error) { return rpc.OK, nil }, rpc.Paired)
	rpc.Add(r, "t.live", func(ctx context.Context, _ rpc.Caller, _ rpc.None) (rpc.Ack, error) { return rpc.OK, ctx.Err() })
	rpc.Add(r, "t.detached", func(ctx context.Context, _ rpc.Caller, _ rpc.None) (rpc.Ack, error) { return rpc.OK, ctx.Err() }, rpc.Detached)
	rpc.Add(r, "t.join", func(context.Context, rpc.Caller, rpc.None) (rpc.Ack, error) { return rpc.OK, nil }, rpc.Unpaired)
	return r
}

var (
	local  = rpc.Caller{Device: "mac", Scope: rpc.ScopeLocal}
	remote = rpc.Caller{Device: "phone", Scope: rpc.ScopeRemote}
)

func TestParamsAndResultAreTyped(t *testing.T) {
	r := router()
	out, err := r.Call(context.Background(), remote, "t.echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil || string(out) != `{"text":"hi"}` {
		t.Fatalf("echo = %s, %v", out, err)
	}
	if _, err := r.Call(context.Background(), remote, "t.echo", json.RawMessage(`{"text":1}`)); !errors.Is(err, rpc.ErrInvalidParams) {
		t.Fatalf("bad params = %v, want invalid", err)
	}
	for _, s := range r.Specs() {
		if s.Name == "t.echo" && (s.Params.Name() != "echo" || s.Result.Name() != "echo") {
			t.Fatalf("spec = %+v", s)
		}
	}
}

func TestAccessIsEnforcedByTheRouter(t *testing.T) {
	r := router()
	for _, tc := range []struct {
		method string
		caller rpc.Caller
		denied bool
	}{
		{"t.local", local, false},
		{"t.local", remote, true},
		{"t.paired", remote, false},
		{"t.paired", local, true},
		{"t.echo", local, false},
		{"t.echo", remote, false},
	} {
		_, err := r.Call(context.Background(), tc.caller, tc.method, nil)
		if denied := rpc.Code(err) == jsonrpc.CodeDenied; denied != tc.denied {
			t.Errorf("%s as scope %d: err %v, want denied=%t", tc.method, tc.caller.Scope, err, tc.denied)
		}
	}
}

// A detached method outlives its caller; any other sees the caller leave.
func TestDetachedIgnoresTheCallersCancel(t *testing.T) {
	r := router()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Call(ctx, remote, "t.live", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("t.live after cancel = %v, want canceled", err)
	}
	if _, err := r.Call(ctx, remote, "t.detached", nil); err != nil {
		t.Fatalf("t.detached after cancel = %v, want it to run", err)
	}
}

// Every kind maps onto its wire code, wrapped or not, and an error without a
// kind is the host's own fault.
func TestCodeMapsKinds(t *testing.T) {
	notFound := errkind.New(errkind.NotFound, "no such thing")
	for _, tc := range []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w x", rpc.ErrUnsupported), jsonrpc.CodeMethodNotFound},
		{fmt.Errorf("%w: bad", rpc.ErrInvalidParams), jsonrpc.CodeInvalidParams},
		{fmt.Errorf("reading: %w", notFound), jsonrpc.CodeNotFound},
		{rpc.ErrLocalOnly, jsonrpc.CodeDenied},
		{rpc.ErrBusy, jsonrpc.CodeUnavailable},
		{errors.New("disk on fire"), jsonrpc.CodeInternal},
	} {
		if got := rpc.Code(tc.err); got != tc.want {
			t.Errorf("Code(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
	if !errors.Is(notFound, rpc.ErrNotFound) || errors.Is(notFound, rpc.ErrDenied) {
		t.Error("a kinded sentinel must match its own kind's root and no other")
	}
}

// Past MaxInflight, a call is refused as busy rather than queued without bound.
func TestInflightBound(t *testing.T) {
	slots := rpc.NewInflight()
	release := make(chan struct{})
	defer close(release)
	for i := range rpc.MaxInflight {
		if !slots.Go(func() { <-release }) {
			t.Fatalf("slot %d refused below the bound", i)
		}
	}
	if slots.Go(func() {}) {
		t.Fatal("a call past the bound was accepted")
	}
	busy := rpc.Busy(&jsonrpc.Message{JSONRPC: jsonrpc.Version, ID: json.RawMessage(`1`), Method: "x"})
	if busy == nil || busy.Error == nil || busy.Error.Code != jsonrpc.CodeUnavailable {
		t.Fatalf("busy reply = %+v, want unavailable", busy)
	}
}

// Only a method registered Unpaired is open to an unpaired device.
func TestUnpairedIsDeclaredOnTheAddLine(t *testing.T) {
	r := router()
	for name, want := range map[string]bool{"t.join": true, "t.echo": false, "t.missing": false} {
		if got := r.Unpaired(name); got != want {
			t.Errorf("Unpaired(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestResultNormalizesRequiredArrays(t *testing.T) {
	type child struct {
		Items []string `json:"items" wire:"array"`
	}
	type result struct {
		Children []child `json:"children" wire:"array"`
	}
	r := router()
	rpc.Add(r, "t.arrays", func(context.Context, rpc.Caller, rpc.None) (result, error) {
		return result{Children: []child{{}}}, nil
	})
	got, err := r.Call(t.Context(), remote, "t.arrays", nil)
	if err != nil || string(got) != `{"children":[{"items":[]}]}` {
		t.Fatalf("result = %s, %v", got, err)
	}
}
