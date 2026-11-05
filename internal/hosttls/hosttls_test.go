package hosttls

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
)

func TestKeyLastsAcrossLoadsAndItsPinVerifies(t *testing.T) {
	host, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tls-key.json")
	first, err := LoadOrCreate(path, host)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(path, host)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh certificate each start, the same key: phones keep their pin.
	if first.SPKI != second.SPKI || first.Pin.Counter != second.Pin.Counter {
		t.Fatalf("key or counter changed across loads: %x %x", first.SPKI, second.SPKI)
	}
	if sha256.Sum256(second.TLS.Leaf.RawSubjectPublicKeyInfo) != second.SPKI {
		t.Fatal("the pin is not the served certificate's key")
	}
	if err := device.VerifyPin(host.Public, second.Pin); err != nil {
		t.Fatalf("pin does not verify: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

func TestRotateServesANewKeyUnderTheNextCounter(t *testing.T) {
	host, _ := device.Generate()
	path := filepath.Join(t.TempDir(), "tls-key.json")
	keys, err := Open(path, host)
	if err != nil {
		t.Fatal(err)
	}
	old := keys.Current()
	next, err := keys.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	if next.SPKI == old.SPKI || next.Pin.Counter <= old.Pin.Counter {
		t.Fatalf("rotate kept the key or the counter: %d → %d", old.Pin.Counter, next.Pin.Counter)
	}
	if device.VerifyPin(host.Public, next.Pin) != nil {
		t.Fatal("the new pin isn't signed by the host")
	}
	served, err := keys.Config().GetCertificate(nil)
	if err != nil || sha256.Sum256(served.Leaf.RawSubjectPublicKeyInfo) != next.SPKI {
		t.Fatal("the config still serves the old key")
	}
	// Saved: a restart serves the rotated key and counter.
	reopened, err := Open(path, host)
	if err != nil || reopened.Current().SPKI != next.SPKI || reopened.Current().Pin.Counter != next.Pin.Counter {
		t.Fatalf("rotation was not saved: %v", err)
	}
}

// A host whose key file was lost starts above every pin its phones hold: the
// counter is never below the time in seconds, and always past the last one.
func TestTheCounterNeverGoesBack(t *testing.T) {
	now := time.Now().Unix()
	if got := nextCounter(0); got < now {
		t.Fatalf("a first key took %d, below the time %d", got, now)
	}
	if got := nextCounter(now + 100); got != now+101 {
		t.Fatalf("a counter ahead of the clock went to %d, want %d", got, now+101)
	}
	host, _ := device.Generate()
	fresh, err := LoadOrCreate(filepath.Join(t.TempDir(), "tls-key.json"), host)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Pin.Counter < now {
		t.Fatalf("a new key file took %d, below the time %d", fresh.Pin.Counter, now)
	}
}
