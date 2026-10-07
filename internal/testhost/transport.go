package testhost

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/hostlink"
	"github.com/repogo/host/internal/relay"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/wsserver"
)

// Token is the loopback token Serve's WebSocket server accepts.
const Token = "test-token"

// Transports is a router served the ways a client reaches it: the loopback
// WebSocket, and a local relay the host has dialled out to.
type Transports struct {
	WS, Relay string // ws:// URLs
	Link      *hostlink.Link

	// Pairing is whether a pairing code is live, as the relay link sees it.
	Pairing atomic.Bool
}

// Relay starts a local relay, shut down when the test ends, and returns its ws:// URL.
func Relay(t testing.TB) string {
	t.Helper()
	rl, err := relay.New(relay.Config{Addr: "127.0.0.1:0", ServerID: "testhost-relay", Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	addr, err := rl.Listen()
	if err != nil {
		t.Fatalf("relay listen: %v", err)
	}
	shutdownAtCleanup(t, rl)
	return fmt.Sprintf("ws://%s/ws", addr.(*net.TCPAddr))
}

func shutdownAtCleanup(t testing.TB, s interface{ Shutdown(context.Context) error }) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
}

// Serve starts both transports for router and returns once the host is
// attached to the relay, since nothing is reachable through it before.
func Serve(t testing.TB, devices *device.Store, router *rpc.Router) *Transports {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tr := &Transports{}

	srv, err := wsserver.New(wsserver.Config{
		Token: Token, ServerID: "testhost", Devices: devices, Router: router,
		Pushes: emit.NewMux(), OnDisconnect: func(device.ID) {}, Log: log,
	})
	if err != nil {
		t.Fatalf("wsserver: %v", err)
	}
	wsAddr, err := srv.Listen()
	if err != nil {
		t.Fatalf("wsserver listen: %v", err)
	}
	shutdownAtCleanup(t, srv)
	tr.WS = fmt.Sprintf("ws://%s/ws", wsAddr.(*net.TCPAddr))

	tr.Relay = Relay(t)

	tr.Link, err = hostlink.New(hostlink.Config{
		URL: tr.Relay, Devices: devices, Router: router, Log: log,
		Pairing: tr.Pairing.Load, OnDisconnect: func(device.ID) {},
	})
	if err != nil {
		t.Fatalf("hostlink: %v", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go tr.Link.Run(ctx)
	select {
	case <-tr.Link.Attached():
	case <-time.After(10 * time.Second):
		t.Fatal("the host never attached to the relay")
	}
	return tr
}
