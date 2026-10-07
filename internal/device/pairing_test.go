package device

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openPairer(t *testing.T) (*Pairer, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pair-reusable.json")
	p, err := OpenPairer(store, path)
	if err != nil {
		t.Fatal(err)
	}
	return p, path
}

func newJoiner(t *testing.T, label string) Peer {
	t.Helper()
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	return Peer{ID: id.ID, Public: id.Public, Label: label, Platform: "ios"}
}

// A reviewer pairs more than one device, days after the code was made.
func TestReusableInviteAdmitsManyDevices(t *testing.T) {
	p, _ := openPairer(t)
	invite, err := p.BeginReusable("wss://relay.example/ws", 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"iPhone", "iPad"} {
		joiner := newJoiner(t, label)
		hostProof, err := p.Complete(joiner, Proof(invite.Code, joiner.Public))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if string(hostProof) != string(Proof(invite.Code, p.store.Identity().Public)) {
			t.Fatalf("%s: host proof is not keyed by the reusable code", label)
		}
	}
	if got := len(p.store.Peers()); got != 2 {
		t.Fatalf("paired %d devices, want 2", got)
	}
}

// A one-time pairing at the machine neither ends the reusable invite nor
// becomes reusable itself.
func TestOneTimePairingBesideReusable(t *testing.T) {
	p, _ := openPairer(t)
	reusable, err := p.BeginReusable("addr", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	once, err := p.Begin("addr")
	if err != nil {
		t.Fatal(err)
	}
	first := newJoiner(t, "first")
	if _, err := p.Complete(first, Proof(once.Code, first.Public)); err != nil {
		t.Fatalf("one-time code: %v", err)
	}
	second := newJoiner(t, "second")
	if _, err := p.Complete(second, Proof(once.Code, second.Public)); !errors.Is(err, ErrBadProof) {
		t.Fatalf("one-time code reused: err = %v, want ErrBadProof", err)
	}
	if _, err := p.Complete(second, Proof(reusable.Code, second.Public)); err != nil {
		t.Fatalf("reusable code after a one-time pairing: %v", err)
	}
}

func TestReusableInviteEnds(t *testing.T) {
	p, path := openPairer(t)
	invite, err := p.BeginReusable("addr", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	joiner := newJoiner(t, "late")

	p.reusable.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	if _, err := p.Complete(joiner, Proof(invite.Code, joiner.Public)); !errors.Is(err, ErrPairingExpired) {
		t.Fatalf("expired: err = %v, want ErrPairingExpired", err)
	}
	if !p.ReusableExpires().IsZero() {
		t.Fatal("an expired invite still reports an expiry")
	}

	invite, err = p.BeginReusable("addr", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RevokeReusable(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(joiner, Proof(invite.Code, joiner.Public)); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("revoked: err = %v, want ErrNoPairing", err)
	}
	reopened, err := OpenPairer(p.store, path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.ReusableExpires().IsZero() {
		t.Fatal("a revoked invite came back after a restart")
	}
}

// The container restarting mid-review must not end the invite.
func TestReusableInviteSurvivesRestart(t *testing.T) {
	p, path := openPairer(t)
	invite, err := p.BeginReusable("addr", 3*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPairer(p.store, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.ReusableExpires().UnixMilli(); got != invite.ExpiresAt {
		t.Fatalf("expires = %d after restart, want %d", got, invite.ExpiresAt)
	}
	joiner := newJoiner(t, "after restart")
	if _, err := reopened.Complete(joiner, Proof(invite.Code, joiner.Public)); err != nil {
		t.Fatalf("after restart: %v", err)
	}
}

func TestReusableInviteBounds(t *testing.T) {
	p, _ := openPairer(t)
	for _, ttl := range []time.Duration{0, time.Minute, MaxReusableTTL + time.Hour} {
		if _, err := p.BeginReusable("addr", ttl); !errors.Is(err, ErrReusableTTL) {
			t.Errorf("ttl %v: err = %v, want ErrReusableTTL", ttl, err)
		}
	}
}
