package account

import (
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/device"
)

const nonce = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func open(t *testing.T) (*Service, *device.Identity, string) {
	t.Helper()
	id, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "account.json")
	s, err := Open(path, id)
	if err != nil {
		t.Fatal(err)
	}
	return s, id, path
}

func TestClaimSignsWhatRepogoAppVerifies(t *testing.T) {
	s, id, _ := open(t)
	p, err := s.Claim("uid_1", nonce)
	if err != nil {
		t.Fatal(err)
	}
	if p.HostID != id.ID || device.IDFor(p.PublicKey) != p.HostID {
		t.Fatalf("host id %s does not derive from the key", p.HostID)
	}
	// The exact bytes server/hostClaim.ts claimMessage builds.
	msg := []byte("repogo-host-claim/1|uid_1|" + nonce + "|" + string(id.ID))
	if !ed25519.Verify(p.PublicKey, msg, p.Signature) {
		t.Fatal("signature does not verify")
	}
}

func TestOneAccountUntilReleasedAtTheMachine(t *testing.T) {
	s, id, path := open(t)
	if _, err := s.Claim("first", nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("first", nonce); err != nil {
		t.Fatalf("same account again: %v", err)
	}
	if _, err := s.Claim("second", nonce); !errors.Is(err, ErrClaimedElsewhere) {
		t.Fatalf("second account: %v", err)
	}
	// Survives a restart.
	again, err := Open(path, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.Claim("second", nonce); !errors.Is(err, ErrClaimedElsewhere) {
		t.Fatalf("second account after restart: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := again.Claim("second", nonce); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if again.Current().UID != "second" {
		t.Fatalf("linked to %q", again.Current().UID)
	}
}

func TestClaimRefusesMalformedInput(t *testing.T) {
	s, _, _ := open(t)
	for _, c := range [][2]string{
		{"", nonce},
		{"a|b", nonce},
		{strings.Repeat("a", 129), nonce},
		{"uid", "short"},
		{"uid", nonce[:42] + "|"},
	} {
		if _, err := s.Claim(c[0], c[1]); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q %q: %v", c[0], c[1], err)
		}
	}
	if s.Current().UID != "" {
		t.Fatal("a refused claim was remembered")
	}
}
