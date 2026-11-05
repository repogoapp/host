package host

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/hosttls"
	"github.com/repogoapp/host/internal/wsserver"
)

// Rotating the TLS key ends the reusable invite: it carries the old pin, so a
// phone pairing from it would pin the retired key. Invites made after carry
// the new one.
func TestRotatingTheTLSKeyEndsTheReusableInvite(t *testing.T) {
	dir := t.TempDir()
	store, err := device.Open(filepath.Join(dir, "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	pairer, err := device.OpenPairer(store, filepath.Join(dir, "pair-reusable.json"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := hosttls.Open(filepath.Join(dir, "tls-key.json"), store.Identity())
	if err != nil {
		t.Fatal(err)
	}
	pairer.SetTLSPin(keys.Current().Pin)
	old, err := pairer.BeginReusable("wss://relay.test/ws", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &Host{log: slog.New(slog.DiscardHandler), pairer: pairer, hostTLS: keys, srv: &wsserver.Server{}}
	next, err := h.rotateTLS()
	if err != nil {
		t.Fatal(err)
	}
	joiner, _ := device.Generate()
	peer := device.Peer{ID: joiner.ID, Public: joiner.Public, Client: device.Client{Platform: "ios"}}
	if _, err := pairer.Complete(peer, device.Proof(old.Code, joiner.Public)); !errors.Is(err, device.ErrNoPairing) {
		t.Fatalf("the reusable invite still pairs after rotation: %v", err)
	}
	fresh, err := pairer.BeginReusable("wss://relay.test/ws", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.TLSPin == nil || fresh.TLSPin.Counter != next.Counter {
		t.Fatalf("a new invite carries %+v, want the rotated pin %d", fresh.TLSPin, next.Counter)
	}
}
