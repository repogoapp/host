package device

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// The bytes are fixed: ChallengeTests.swift signs the same vector, so the
// phone and the relay agree on what a login signature covers.
func TestChallengeMessageVector(t *testing.T) {
	nonce := make([]byte, NonceLen)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	got, err := ChallengeMessage(nonce, "relay", 1700000000000)
	if err != nil {
		t.Fatal(err)
	}
	want := "7265706f676f2d6c6f67696e2d7631" + // "repogo-login-v1"
		hex.EncodeToString(nonce) +
		"0005" + "72656c6179" + // len("relay"), "relay"
		"0000018bcfe56800" // 1700000000000, big-endian
	if hex.EncodeToString(got) != want {
		t.Fatalf("got %x\nwant %s", got, want)
	}
}

func TestChallengeMessageStartsWithItsTag(t *testing.T) {
	// A server picks the nonce; the message must open with the signer's tag
	// regardless, or a nonce could make a login signature cover another
	// protocol's message.
	nonce := bytes.Repeat([]byte("repogo-noise-static-v1"), 2)[:NonceLen]
	got, err := ChallengeMessage(nonce, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, []byte(challengeTag)) {
		t.Fatalf("message %q does not start with %q", got, challengeTag)
	}
}

func TestChallengeMessageRefusesOtherNonceSizes(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := ChallengeMessage(make([]byte, n), "relay", 1); !errors.Is(err, ErrBadNonce) {
			t.Errorf("nonce of %d bytes: got %v, want ErrBadNonce", n, err)
		}
	}
}

// Fixed bytes: PushGrantTests.swift signs the same vector, so the phone and
// the relay agree on what a push grant covers.
func TestPushGrantMessageVector(t *testing.T) {
	got := PushGrantMessage("1003b698e1b7b906d8099bd95f5866fd", "abcd", "sandbox", 1700000000000)
	want := "7265706f676f2d707573682d6772616e742d7631" + // "repogo-push-grant-v1"
		"0020" + hex.EncodeToString([]byte("1003b698e1b7b906d8099bd95f5866fd")) +
		"0004" + "61626364" + // "abcd"
		"0007" + "73616e64626f78" + // "sandbox"
		"0000018bcfe56800"
	if hex.EncodeToString(got) != want {
		t.Fatalf("got %x\nwant %s", got, want)
	}
}
