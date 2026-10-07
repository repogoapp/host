package device

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/errkind"
)

// Pairing has to outlive the process, or every update would mean re-pairing
// every phone.
func TestPairingSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.json")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := IDFor(pub)
	if err := first.Add(Peer{ID: id, Public: pub, Label: "Phone", Platform: "ios"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	identity, group := first.Identity().ID, first.GroupID()

	// A different Store over the same file: what the next `go run` sees.
	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	// The host keeping its own identity matters as much as keeping the peer:
	// a new keypair would invalidate every pairing at once.
	if second.Identity().ID != identity {
		t.Errorf("host identity changed across restart: %s -> %s", identity, second.Identity().ID)
	}
	if second.GroupID() != group {
		t.Errorf("group changed across restart: %s -> %s", group, second.GroupID())
	}

	p, err := second.Peer(id)
	if err != nil {
		t.Fatalf("paired device missing after restart: %v", err)
	}
	if p.Label != "Phone" {
		t.Errorf("label = %q, want %q", p.Label, "Phone")
	}
}

// A removed device must stay removed across a restart, or a host update would
// silently readmit every phone the user removed. Its push targets go with it.
func TestRevocationSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.json")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id := addPeer(t, first, "Old phone")
	if err := first.RegisterPush(id, "", "", PushTarget{Token: "ab", Environment: "sandbox"}); err != nil {
		t.Fatalf("register push: %v", err)
	}
	var told []ID
	first.OnRevoke(func(id ID) { told = append(told, id) })
	if err := first.Revoke("", id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if len(told) != 1 || told[0] != id {
		t.Errorf("revoke listeners heard %v, want [%s]", told, id)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := second.Peer(id); !errors.Is(err, ErrUnknownDevice) {
		t.Errorf("a removed device came back: %v", err)
	}
	if len(second.Peers()) != 0 {
		t.Errorf("a removed device is listed after restart")
	}
}

// Re-pairing the same phone leaves its old records active, and each carried
// the phone's token, so every push arrived once per pairing. The newest
// registration owns the token.
func TestPushTokenBelongsToOnePeer(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	old := addPeer(t, s, "Old pairing")
	fresh := addPeer(t, s, "New pairing")
	other := addPeer(t, s, "Other phone")

	phone := PushTarget{Token: "a1b2", Environment: "production"}
	for _, id := range []ID{old, fresh} {
		if err := s.RegisterPush(id, "", "", phone); err != nil {
			t.Fatalf("register push: %v", err)
		}
		if err := s.RegisterPush(id, PushToStart, "", phone); err != nil {
			t.Fatalf("register push-to-start: %v", err)
		}
	}
	if err := s.RegisterPush(other, "", "", PushTarget{Token: "c3d4", Environment: "sandbox"}); err != nil {
		t.Fatalf("register other: %v", err)
	}

	peer := func(id ID) Peer {
		p, err := s.Peer(id)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := peer(old); p.Push != nil || p.PushToStart != nil {
		t.Error("the old pairing kept the phone's token, so the phone is pushed twice")
	}
	if p := peer(fresh); p.Push == nil || p.PushToStart == nil {
		t.Error("the newest pairing lost the phone's token")
	}
	if p := peer(other); p.Push == nil || p.Push.Token != "c3d4" {
		t.Error("a different phone's token was cleared")
	}
}

// A reinstall mints a new token, so the old pairing's token no longer
// matches; the install ID still ties the two records to one phone.
func TestPushTargetBelongsToOneInstall(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	before := addPeer(t, s, "Before reinstall")
	after := addPeer(t, s, "After reinstall")
	other := addPeer(t, s, "Other phone")

	regs := []struct {
		id     ID
		target PushTarget
	}{
		{before, PushTarget{Token: "e5f6", InstallID: "install-a", Environment: "sandbox"}},
		{other, PushTarget{Token: "c3d4", InstallID: "install-b", Environment: "sandbox"}},
		{after, PushTarget{Token: "0a0b", InstallID: "install-a", Environment: "sandbox"}},
	}
	for _, r := range regs {
		if err := s.RegisterPush(r.id, "", "", r.target); err != nil {
			t.Fatalf("register push: %v", err)
		}
		if err := s.RegisterPush(r.id, PushToStart, "", r.target); err != nil {
			t.Fatalf("register push-to-start: %v", err)
		}
	}

	if p, _ := s.Peer(before); p.Push != nil || p.PushToStart != nil {
		t.Error("the pairing from before the reinstall kept its targets")
	}
	if p, _ := s.Peer(after); p.Push == nil || p.PushToStart == nil {
		t.Error("the newest pairing lost its targets")
	}
	if p, _ := s.Peer(other); p.Push == nil || p.PushToStart == nil {
		t.Error("another install's targets were cleared")
	}
}

func addPeer(t *testing.T, s *Store, label string) ID {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := IDFor(pub)
	if err := s.Add(Peer{ID: id, Public: pub, Label: label}); err != nil {
		t.Fatalf("add %s: %v", label, err)
	}
	return id
}

// A save that fails must not leave memory admitting what the file does not:
// a device paired or revoked only in memory would change on restart.
func TestFailedSaveRollsBack(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	kept := addPeer(t, s, "Kept")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	joiner := Peer{ID: IDFor(pub), Public: pub}

	// The atomic write creates its temp file beside the target; a read-only
	// directory makes every save fail.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if err := s.Add(joiner); err == nil {
		t.Fatal("add saved into a read-only directory")
	}
	if _, err := s.Peer(joiner.ID); !errors.Is(err, ErrUnknownDevice) {
		t.Errorf("an unsaved device was admitted: %v", err)
	}
	if err := s.Revoke("", kept); err == nil {
		t.Fatal("revoke saved into a read-only directory")
	}
	if _, err := s.Peer(kept); err != nil {
		t.Errorf("an unsaved revoke took effect: %v", err)
	}
}

func TestRevokeRefusesItselfAndTheHost(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	phone := addPeer(t, s, "Phone")
	if err := s.Revoke(phone, phone); !errors.Is(err, ErrRevokeSelf) || !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("self revoke: %v", err)
	}
	if err := s.Revoke(phone, s.Identity().ID); !errors.Is(err, ErrRevokeHost) {
		t.Errorf("host revoke: %v", err)
	}
	if err := s.Revoke(phone, "missing"); !errors.Is(err, ErrUnknownDevice) {
		t.Errorf("unknown revoke: %v", err)
	}
}

func TestRegisterPushValidatesAndStamps(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	phone := addPeer(t, s, "Phone")
	bad := []struct {
		kind   string
		target PushTarget
	}{
		{"alert", PushTarget{Token: "ab", Environment: "sandbox"}},
		{"", PushTarget{Token: "not-hex", Environment: "sandbox"}},
		{"", PushTarget{Token: "ab", Environment: "staging"}},
		{"", PushTarget{Token: "ab", Environment: "sandbox", InstallID: strings.Repeat("x", 65)}},
	}
	for _, b := range bad {
		if err := s.RegisterPush(phone, b.kind, "", b.target); !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("%q %+v: %v", b.kind, b.target, err)
		}
	}
	// A client's At is ignored; an activity token gets the host's clock.
	target := PushTarget{Token: "ab", Environment: "sandbox", At: 1}
	if err := s.RegisterPush(phone, "", "", target); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterPush(phone, "", "claude:s1", target); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Peer(phone)
	if p.Push.At != 0 || p.Activities["claude:s1"].At < time.Now().Add(-time.Minute).UnixMilli() {
		t.Errorf("push at %d, activity at %d", p.Push.At, p.Activities["claude:s1"].At)
	}
	if err := s.ForgetPush(phone, "claude:s1"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Peer(phone); len(p.Activities) != 0 {
		t.Errorf("activity kept: %+v", p.Activities)
	}
}
