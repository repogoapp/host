package securechan

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"testing"

	"github.com/repogoapp/host/internal/device"
)

// A phone whose identity is a Secure Enclave P-256 key (plan 520) proves itself
// in message 3 with a 65-byte point and a 64-byte signature; the host takes it,
// and the session's peer is that key.
func TestAP256PhoneOpensAChannel(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := key.PublicKey.Bytes()
	sign := func(msg []byte) []byte {
		digest := sha256.Sum256(msg)
		r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	}
	phoneID := device.IDFor(public)
	host, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}

	toHost, toPhone := make(chan []byte, 4), make(chan []byte, 4)
	hostDone := make(chan *Session, 1)
	hostErr := make(chan error, 1)
	go func() {
		r, msg2, err := Respond(host, phoneID, <-toHost)
		if err != nil {
			hostErr <- err
			return
		}
		toPhone <- msg2
		msg3 := <-toHost
		if len(msg3)-1 != msg3P256Len {
			hostErr <- errLength
			return
		}
		sess, err := r.Finish(msg3)
		hostErr <- err
		hostDone <- sess
	}()
	phone, err := initiate(context.Background(), phoneID, public, sign, host.ID, host.Public,
		func(_ context.Context, b []byte) error { toHost <- b; return nil },
		func(context.Context) ([]byte, error) { return <-toPhone, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := <-hostErr; err != nil {
		t.Fatalf("host: %v", err)
	}
	hostSess := <-hostDone
	if !bytes.Equal(hostSess.Peer, public) {
		t.Fatal("the host's session peer is not the phone's P-256 key")
	}
	got, err := hostSess.Open(seal(t, phone, []byte("hello")))
	if err != nil || string(got) != "hello" {
		t.Fatalf("record = %q, %v", got, err)
	}
}

func TestAProofOfTheWrongLengthIsMalformed(t *testing.T) {
	if _, err := verifyProof(make([]byte, p256ProofLen+1), roleInitiator, nil); err == nil {
		t.Fatal("a proof of neither length was accepted")
	}
}
