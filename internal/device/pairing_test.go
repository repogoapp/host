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
	p, err := OpenPairer(store, path, Label)
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
	return Peer{ID: id.ID, Public: id.Public, Label: label, Client: Client{Platform: "ios"}}
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
	once, err := p.Begin("addr", "")
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

// The reviewer's phone reaches the host over the relay, which admits
// strangers only while Accepting says so.
func TestAcceptingWithOnlyAReusableInvite(t *testing.T) {
	p, _ := openPairer(t)
	if p.Accepting() {
		t.Fatal("accepting with no code or invite")
	}
	if _, err := p.BeginReusable("addr", time.Hour); err != nil {
		t.Fatal(err)
	}
	if !p.Accepting() {
		t.Fatal("not accepting while a reusable invite is live")
	}
	p.reusable.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	if p.Accepting() {
		t.Fatal("accepting after the reusable invite expired")
	}
	if _, err := p.Begin("addr", ""); err != nil {
		t.Fatal(err)
	}
	if !p.Accepting() {
		t.Fatal("not accepting while a one-time code is live")
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
	reopened, err := OpenPairer(p.store, path, Label)
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
	reopened, err := OpenPairer(p.store, path, Label)
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

// The invite names the machine, so a phone opening a link can show whose
// environment it is about to join.
func TestInviteCarriesTheHostLabel(t *testing.T) {
	p, _ := openPairer(t)
	invite, err := p.Begin("ws://127.0.0.1:1/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if invite.Label == "" || invite.Label != Label() {
		t.Fatalf("invite label %q, want the host's %q", invite.Label, Label())
	}
}

// A join reports the invite ID its code was tagged with; a code the CLI
// began after it carries none, so the replaced phone's sheet never hears one.
func TestCompleteReportsInviteID(t *testing.T) {
	p, _ := openPairer(t)
	var heard []string
	p.OnComplete(func(id string) { heard = append(heard, id) })

	tagged, err := p.Begin("addr", "sheet-1")
	if err != nil {
		t.Fatal(err)
	}
	first := newJoiner(t, "iPad")
	if _, err := p.Complete(first, Proof(tagged.Code, first.Public)); err != nil {
		t.Fatal(err)
	}

	p.Begin("addr", "sheet-2")
	untagged, _ := p.Begin("addr", "")
	second := newJoiner(t, "iPhone")
	if _, err := p.Complete(second, Proof(untagged.Code, second.Public)); err != nil {
		t.Fatal(err)
	}
	if len(heard) != 2 || heard[0] != "sheet-1" || heard[1] != "" {
		t.Fatalf("heard %q, want [sheet-1 \"\"]", heard)
	}
}

// Cancel ends only the code it names: one the CLI began since is left live.
func TestCancelLeavesNewerCode(t *testing.T) {
	p, _ := openPairer(t)
	stale, _ := p.Begin("addr", "sheet")
	p.Begin("addr", "")
	p.Cancel(stale.Code)
	if pending, _ := p.Pending(); !pending {
		t.Fatal("cancelling a replaced code ended the newer one")
	}
	current, _ := p.Begin("addr", "sheet")
	p.Cancel(current.Code)
	if pending, _ := p.Pending(); pending {
		t.Fatal("cancel left its own code live")
	}
}

// Revoking a device ends the one-time code too: one the device asked for
// before it was revoked can't pair it, or anyone, back in.
func TestRemoveEndsAPendingOneTimeCode(t *testing.T) {
	p, _ := openPairer(t)
	phone := newJoiner(t, "phone")
	if err := p.store.Add(phone); err != nil {
		t.Fatal(err)
	}
	once, err := p.Begin("addr", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Remove(p.store.Identity().ID, phone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(phone, Proof(once.Code, phone.Public)); err == nil {
		t.Fatal("the revoked phone paired back in with its code")
	}
	if p.Accepting() {
		t.Fatal("still accepting after the revoke")
	}
}
