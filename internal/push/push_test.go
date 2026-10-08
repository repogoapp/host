package push

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/power"
	"github.com/repogo/host/internal/push/pushtest"
)

func storeWithPhones(t *testing.T) (*device.Store, device.ID) {
	t.Helper()
	store, err := device.Open(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	withToken, _ := device.Generate()
	silent, _ := device.Generate()
	for _, id := range []*device.Identity{withToken, silent} {
		if err := store.Add(device.Peer{ID: id.ID, Public: id.Public}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RegisterPush(withToken.ID, "", "", withToken.GrantPush(store.Identity().ID, device.PushTarget{Token: "abcd", Environment: "sandbox"})); err != nil {
		t.Fatal(err)
	}
	return store, withToken.ID
}

func TestOnlyPeersWithTokensAreNotified(t *testing.T) {
	store, _ := storeWithPhones(t)
	sender := &pushtest.Sender{}
	n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))

	n.deliver(context.Background(), notify.Notice{Agent: agent.Kind("claude"), SessionID: "s1"},
		alert{"Task finished", "Your environment finished a task."})

	if len(sender.Calls) != 1 {
		t.Fatalf("sent %d pushes, want 1", len(sender.Calls))
	}
	call := sender.Calls[0]
	if call.Token != "abcd" || call.Environment != "sandbox" || call.CollapseID != "claude:s1" {
		t.Fatalf("push = %+v", call)
	}
	payload := string(call.Payload)
	for _, want := range []string{`"title":"Task finished"`, `"thread-id":"claude:s1"`, `"chat_id":"claude:s1"`, `"host_id":"` + string(store.Identity().ID) + `"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload lacks %s: %s", want, payload)
		}
	}
	// The relay and Apple read this; nothing about the work may be in it.
	if strings.Contains(payload, "cwd") || strings.Contains(payload, "message") {
		t.Errorf("payload leaks work details: %s", payload)
	}
}

func TestDeadTokenIsForgotten(t *testing.T) {
	store, id := storeWithPhones(t)
	sender := &pushtest.Sender{Reply: &jsonrpc.Error{Code: jsonrpc.CodeNotFound, Message: "apns: unregistered"}}
	n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))

	n.deliver(context.Background(), notify.Notice{Agent: agent.Kind("claude"), SessionID: "s1"}, alert{"x", "y"})

	if peer, _ := store.Peer(id); peer.Push != nil {
		t.Fatal("dead token still stored")
	}
}

func TestOnlyBlockingStatusesPush(t *testing.T) {
	pushes := map[agent.ChatStatus]bool{
		agent.ChatAwaitingApproval: true, agent.ChatAwaitingUser: true,
		agent.ChatCompleted: false, agent.ChatFailed: false,
		agent.ChatWorking: false, agent.ChatQueued: false, agent.ChatIdle: false,
		agent.ChatCancelled: false, agent.ChatInterrupted: false,
	}
	for status, want := range pushes {
		if _, got := alertFor(status); got != want {
			t.Errorf("%s pushes = %v, want %v", status, got, want)
		}
	}
}

func TestLowBatteryAlertsOnceUntilRecovered(t *testing.T) {
	store, _ := storeWithPhones(t)
	sender := &pushtest.Sender{}
	n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	on := func(percent int) power.Battery { return power.Battery{Present: true, Percent: percent} }

	for _, b := range []power.Battery{on(20), on(10), on(9), on(8), on(12)} {
		n.Battery(ctx, b)
	}
	if len(sender.Calls) != 1 {
		t.Fatalf("sent %d pushes draining to 8%%, want 1", len(sender.Calls))
	}
	call := sender.Calls[0]
	if call.CollapseID != "battery" || !strings.Contains(string(call.Payload), `"title":"Low battery"`) ||
		!strings.Contains(string(call.Payload), "9%") {
		t.Fatalf("push = %+v, payload %s", call, call.Payload)
	}

	// Back above the re-arm line, then down again: a second alert.
	n.Battery(ctx, on(16))
	n.Battery(ctx, on(5))
	// Plugged in re-arms too; charging at a low level never alerts.
	n.Battery(ctx, power.Battery{Present: true, Percent: 5, Charging: true, PluggedIn: true})
	n.Battery(ctx, power.Battery{Present: true, Percent: 3, PluggedIn: true})
	n.Battery(ctx, on(3))
	// A desktop Mac has no battery to run low.
	n.Battery(ctx, power.Battery{PluggedIn: true})
	if len(sender.Calls) != 3 {
		t.Fatalf("sent %d pushes, want 3", len(sender.Calls))
	}
}

func TestEnvRequestCarriesItsID(t *testing.T) {
	store, _ := storeWithPhones(t)
	sender := &pushtest.Sender{}
	n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))

	n.EnvRequest(context.Background(), "req-1", "Studio", []string{"api-keys", "db"})

	if len(sender.Calls) != 1 {
		t.Fatalf("sent %d pushes, want 1", len(sender.Calls))
	}
	payload := string(sender.Calls[0].Payload)
	for _, want := range []string{`"title":"Share secrets?"`, `"body":"Studio wants api-keys, db"`, `"kind":"env_request"`, `"request_id":"req-1"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload lacks %s: %s", want, payload)
		}
	}
}
