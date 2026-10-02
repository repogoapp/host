package securechan

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"testing"

	"github.com/flynn/noise"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/jsonrpc"
)

// pipe runs the initiator and responder against each other in memory,
// returning both sessions.
func pipe(t *testing.T, phone, host *device.Identity, hostPin ed25519.PublicKey, claimedPhone device.ID) (*Session, *Session, error) {
	t.Helper()
	toHost, toPhone := make(chan []byte, 4), make(chan []byte, 4)
	send := func(_ context.Context, b []byte) error { toHost <- b; return nil }
	recv := func(context.Context) ([]byte, error) { return <-toPhone, nil }

	hostErr := make(chan error, 1)
	var hostSess *Session
	go func() {
		r, msg2, err := Respond(host, claimedPhone, <-toHost)
		if err != nil {
			hostErr <- err
			return
		}
		toPhone <- msg2
		hostSess, err = r.Finish(<-toHost)
		hostErr <- err
	}()
	phoneSess, err := Initiate(context.Background(), phone, host.ID, hostPin, send, recv)
	if err != nil {
		return nil, nil, err
	}
	if err := <-hostErr; err != nil {
		return nil, nil, err
	}
	return phoneSess, hostSess, nil
}

func identities(t *testing.T) (phone, host *device.Identity) {
	t.Helper()
	phone, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	host, err = device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return phone, host
}

func seal(t *testing.T, s *Session, plain []byte) []byte {
	t.Helper()
	var wire []byte
	if err := s.Seal(plain, func(b []byte) error { wire = b; return nil }); err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestRoundTrip(t *testing.T) {
	phone, host := identities(t)
	ps, hs, err := pipe(t, phone, host, host.Public, phone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Peer.Equal(host.Public) || !hs.Peer.Equal(phone.Public) {
		t.Fatal("sessions did not learn the right peer identities")
	}
	for i, msg := range [][]byte{[]byte(`{"a":1}`), nil, bytes.Repeat([]byte("x"), 1<<20)} {
		got, err := hs.Open(seal(t, ps, msg))
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("phone→host %d: %v", i, err)
		}
		got, err = ps.Open(seal(t, hs, msg))
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("host→phone %d: %v", i, err)
		}
	}
}

func TestTamperAndReplayAreRejected(t *testing.T) {
	phone, host := identities(t)
	ps, hs, err := pipe(t, phone, host, host.Public, phone.ID)
	if err != nil {
		t.Fatal(err)
	}
	wire := seal(t, ps, []byte("hello"))
	flipped := append([]byte(nil), wire...)
	flipped[len(flipped)-1] ^= 1
	if _, err := hs.Open(flipped); !errors.Is(err, errAuth) {
		t.Fatalf("tampered record: %v", err)
	}
	// The counter advanced on the failure, so even the honest copy is dead:
	// a failed open ends the session.
	if _, err := hs.Open(wire); err == nil {
		t.Fatal("record accepted after a failed open")
	}
}

func TestReorderIsRejected(t *testing.T) {
	phone, host := identities(t)
	ps, hs, err := pipe(t, phone, host, host.Public, phone.ID)
	if err != nil {
		t.Fatal(err)
	}
	seal(t, ps, []byte("first"))
	second := seal(t, ps, []byte("second"))
	if _, err := hs.Open(second); !errors.Is(err, errAuth) {
		t.Fatalf("record opened out of order: %v", err)
	}
}

// The last counter value is reserved (Noise §5.1): neither direction uses it.
func TestSpentCounterRefuses(t *testing.T) {
	var c cipherState
	if err := c.init(bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	c.n = math.MaxUint64 - 1
	if _, err := c.encrypt(nil, []byte("last")); err != nil {
		t.Fatalf("second-to-last counter: %v", err)
	}
	if _, err := c.encrypt(nil, []byte("reserved")); !errors.Is(err, errSpent) {
		t.Fatalf("sealed at the reserved counter: %v", err)
	}
	if _, err := c.decrypt(nil, make([]byte, tagLen)); !errors.Is(err, errSpent) {
		t.Fatalf("opened at the reserved counter: %v", err)
	}
}

// Past maxKeyBytes the receiver refuses, which drops the session like any
// failed open, and the phone handshakes again under fresh keys.
func TestKeyByteCapRefuses(t *testing.T) {
	phone, host := identities(t)
	ps, hs, err := pipe(t, phone, host, host.Public, phone.ID)
	if err != nil {
		t.Fatal(err)
	}
	wire := seal(t, ps, []byte("under the cap"))
	hs.recv.opened = maxKeyBytes - uint64(len(wire)-1)
	if _, err := hs.Open(wire); err != nil {
		t.Fatalf("record that reaches the cap exactly: %v", err)
	}
	if _, err := hs.Open(seal(t, ps, []byte("over"))); !errors.Is(err, errSpent) {
		t.Fatalf("opened past the cap: %v", err)
	}
}

func TestWrongPinFails(t *testing.T) {
	phone, host := identities(t)
	other, _ := identities(t)
	if _, _, err := pipe(t, phone, host, other.Public, phone.ID); err == nil {
		t.Fatal("phone accepted a host whose key is not the pinned one")
	}
}

func TestSpoofedSenderIDFails(t *testing.T) {
	phone, host := identities(t)
	other, _ := identities(t)
	// The relay says the caller is `other`; the proof inside says `phone`.
	if _, _, err := pipe(t, phone, host, host.Public, other.ID); err == nil {
		t.Fatal("host bound a session to an id its identity does not fingerprint")
	}
}

func TestWrongPrologueFails(t *testing.T) {
	phone, host := identities(t)
	other, _ := identities(t)
	toHost, toPhone := make(chan []byte, 4), make(chan []byte, 4)
	go func() {
		// Host believes it is talking to `other`, so the prologue differs.
		_, msg2, err := Respond(host, other.ID, <-toHost)
		if err != nil {
			return
		}
		toPhone <- msg2
	}()
	_, err := Initiate(context.Background(), phone, host.ID, host.Public,
		func(_ context.Context, b []byte) error { toHost <- b; return nil },
		func(context.Context) ([]byte, error) { return <-toPhone, nil })
	if err == nil {
		t.Fatal("handshake succeeded across mismatched prologues")
	}
}

// flynnKey adapts a key for flynn/noise, which plays a phone on the old suite.
func flynnKey(t *testing.T, k *ecdh.PrivateKey) noise.DHKey {
	t.Helper()
	return noise.DHKey{Private: k.Bytes(), Public: k.PublicKey().Bytes()}
}

// A phone still on plain XX sends a 32-byte message 1, which the host refuses
// by length before any key is derived: no fallback.
func TestOldSuiteFails(t *testing.T) {
	old, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherAESGCM, noise.HashSHA256),
		Pattern:     noise.HandshakeXX, Prologue: []byte("prologue"), Initiator: true,
		StaticKeypair: flynnKey(t, generateKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	msg1, _, _, err := old.WriteMessage(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	host := newHandshake(false, []byte("prologue"), generateKey(), generateKey(), nil)
	if err := host.readMessage1(msg1); !errors.Is(err, errLength) {
		t.Fatalf("host accepted a plain XX message 1: %v", err)
	}
}

// Noise §5.2: the suite name is longer than a hash, so it is hashed; a short
// name is zero-padded.
func TestInitialHash(t *testing.T) {
	if len(protocolName) <= sha256.Size {
		t.Fatalf("%q fits in a hash; this test expects the hashed branch", protocolName)
	}
	if want := sha256.Sum256([]byte(protocolName)); !bytes.Equal(initialHash(protocolName), want[:]) {
		t.Fatal("a long protocol name was not hashed")
	}
	if want := append([]byte("Noise_XX"), make([]byte, 24)...); !bytes.Equal(initialHash("Noise_XX"), want) {
		t.Fatal("a short protocol name was not zero-padded")
	}
}

// handshakePair starts a fresh initiator and responder with random keys.
func handshakePair() (phone, host *handshakeState) {
	pro := []byte("prologue")
	return newHandshake(true, pro, generateKey(), generateKey(), generateKEMKey()),
		newHandshake(false, pro, generateKey(), generateKey(), nil)
}

func TestExactLengths(t *testing.T) {
	phone, host := handshakePair()
	msg1, err := phone.writeMessage1()
	if err != nil {
		t.Fatal(err)
	}
	if len(msg1) != msg1Len || Message1Len != 1601 {
		t.Fatalf("message 1 is %d bytes (framed %d), want 1600 (1601)", len(msg1), Message1Len)
	}
	for _, bad := range [][]byte{msg1[:len(msg1)-1], append(append([]byte(nil), msg1...), 0)} {
		_, fresh := handshakePair()
		if err := fresh.readMessage1(bad); !errors.Is(err, errLength) {
			t.Fatalf("message 1 of %d bytes: %v", len(bad), err)
		}
	}
	if err := host.readMessage1(msg1); err != nil {
		t.Fatal(err)
	}
	msg2, err := host.writeMessage2(make([]byte, proofLen))
	if err != nil {
		t.Fatal(err)
	}
	if len(msg2) != 1776 {
		t.Fatalf("message 2 is %d bytes, want 1776", len(msg2))
	}
	if _, err := phone.readMessage2(append(msg2, 0)); !errors.Is(err, errLength) {
		t.Fatalf("message 2 with a trailing byte: %v", err)
	}
	if _, err := phone.readMessage2(msg2); err != nil {
		t.Fatal(err)
	}
	msg3, err := phone.writeMessage3(make([]byte, proofLen))
	if err != nil {
		t.Fatal(err)
	}
	if len(msg3) != 160 {
		t.Fatalf("message 3 is %d bytes, want 160", len(msg3))
	}
	if _, err := host.readMessage3(msg3[:len(msg3)-1]); !errors.Is(err, errLength) {
		t.Fatalf("short message 3: %v", err)
	}
}

// An e1 that is not a valid ML-KEM-1024 key is refused before any use.
func TestInvalidKEMKeyIsRefused(t *testing.T) {
	phone, host := handshakePair()
	msg1, err := phone.writeMessage1()
	if err != nil {
		t.Fatal(err)
	}
	for i := dhLen; i < len(msg1); i++ {
		msg1[i] = 0xff
	}
	if err := host.readMessage1(msg1); !errors.Is(err, errLength) {
		t.Fatalf("host took an invalid ML-KEM key: %v", err)
	}
}

// One coefficient at q = 3329 fails FIPS 203's key check, which Go's
// import runs; SecureChannelTests checks Swift refuses the same key.
func TestOutOfRangeKEMCoefficientIsRefused(t *testing.T) {
	phone, host := handshakePair()
	msg1, err := phone.writeMessage1()
	if err != nil {
		t.Fatal(err)
	}
	msg1[dhLen] = 0x01
	msg1[dhLen+1] = msg1[dhLen+1]&0xf0 | 0x0d
	if err := host.readMessage1(msg1); !errors.Is(err, errLength) {
		t.Fatalf("host took a key with a coefficient at q: %v", err)
	}
}

// A handshake run in the wrong role or order errors instead of panicking.
func TestStepsOutOfOrderError(t *testing.T) {
	phone, host := handshakePair()
	if _, err := host.writeMessage1(); !errors.Is(err, errOrder) {
		t.Fatalf("responder wrote message 1: %v", err)
	}
	if _, err := host.writeMessage2(nil); !errors.Is(err, errOrder) {
		t.Fatalf("responder wrote message 2 before message 1: %v", err)
	}
	if _, err := host.readMessage2(make([]byte, msg2Len)); !errors.Is(err, errOrder) {
		t.Fatalf("responder read message 2: %v", err)
	}
	if _, err := host.writeMessage3(nil); !errors.Is(err, errOrder) {
		t.Fatalf("message 3 written before message 2: %v", err)
	}
	msg1, _ := phone.writeMessage1()
	if err := host.readMessage1(msg1); err != nil {
		t.Fatal(err)
	}
	msg2, err := host.writeMessage2(make([]byte, proofLen))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := phone.readMessage2(msg2); err != nil {
		t.Fatal(err)
	}
	if phone.e1 != nil || host.re1 != nil {
		t.Fatal("an ML-KEM key outlived its use")
	}
}

// The ciphertext travels encrypted under the ee key, so tampering with it is
// caught by that tag before decapsulation.
func TestTamperedKEMCiphertextFails(t *testing.T) {
	phone, host := handshakePair()
	msg1, _ := phone.writeMessage1()
	if err := host.readMessage1(msg1); err != nil {
		t.Fatal(err)
	}
	msg2, err := host.writeMessage2(make([]byte, proofLen))
	if err != nil {
		t.Fatal(err)
	}
	msg2[dhLen+100] ^= 1
	if _, err := phone.readMessage2(msg2); !errors.Is(err, errAuth) {
		t.Fatalf("tampered ciphertext: %v", err)
	}
}

// A corrupted ciphertext sealed under the ee key passes that tag, and ML-KEM's
// implicit rejection decapsulates it to a different secret with no error: the
// next encrypted field is what fails.
func TestImplicitRejectionFailsNextOpen(t *testing.T) {
	phone, host := handshakePair()
	msg1, _ := phone.writeMessage1()
	if err := host.readMessage1(msg1); err != nil {
		t.Fatal(err)
	}
	host.encapsulate = func(ek *mlkem.EncapsulationKey1024) ([]byte, []byte) {
		secret, ct := ek.Encapsulate()
		ct[0] ^= 1
		return secret, ct
	}
	msg2, err := host.writeMessage2(make([]byte, proofLen))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := phone.readMessage2(msg2); !errors.Is(err, errAuth) {
		t.Fatalf("corrupted KEM ciphertext: %v", err)
	}
}

// Known-answer vector shared with SecureChannelTests.swift: keys from fixed
// seeds, plus one pinned encapsulation (secret and ciphertext), which each
// side's own ML-KEM must recover, since encapsulation is randomized.
func TestKnownAnswer(t *testing.T) {
	seed := func(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }
	x := func(b byte) *ecdh.PrivateKey {
		k, err := ecdh.X25519().NewPrivateKey(seed(b))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	phoneID, hostID := ed25519.NewKeyFromSeed(seed(0x11)), ed25519.NewKeyFromSeed(seed(0x22))
	phonePub, hostPub := phoneID.Public().(ed25519.PublicKey), hostID.Public().(ed25519.PublicKey)
	pro, err := prologueFor(device.IDFor(phonePub), device.IDFor(hostPub))
	if err != nil {
		t.Fatal(err)
	}
	e1, err := mlkem.NewDecapsulationKey1024(bytes.Repeat([]byte{0x05}, 64))
	if err != nil {
		t.Fatal(err)
	}
	ct, err := hex.DecodeString(knownCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	kemKey, err := hex.DecodeString(knownKEMSecret)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e1.Decapsulate(ct); err != nil || !bytes.Equal(got, kemKey) {
		t.Fatalf("the pinned ciphertext does not decapsulate to its secret: %x %v", got, err)
	}

	// Go's Ed25519 signatures are deterministic, so the proofs are fixed too;
	// the Swift test sends these exact bytes to pin every message whole.
	hostProof := proof(func(m []byte) []byte { return ed25519.Sign(hostID, m) }, hostPub, x(0x03))
	phoneProof := proof(func(m []byte) []byte { return ed25519.Sign(phoneID, m) }, phonePub, x(0x01))

	phone := newHandshake(true, pro, x(0x01), x(0x02), e1)
	host := newHandshake(false, pro, x(0x03), x(0x04), nil)
	host.encapsulate = func(*mlkem.EncapsulationKey1024) ([]byte, []byte) { return kemKey, ct }

	msg1, err := phone.writeMessage1()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.readMessage1(msg1); err != nil {
		t.Fatal(err)
	}
	msg2, err := host.writeMessage2(hostProof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := phone.readMessage2(msg2); err != nil {
		t.Fatal(err)
	}
	msg3, err := phone.writeMessage3(phoneProof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.readMessage3(msg3); err != nil {
		t.Fatal(err)
	}
	ps, pr, err := phone.split()
	if err != nil {
		t.Fatal(err)
	}
	hs, hr, err := host.split()
	if err != nil {
		t.Fatal(err)
	}
	rec1, _ := ps.encrypt(nil, []byte("phone→host"))
	rec2, _ := hs.encrypt(nil, []byte("host→phone"))
	if _, err := hr.decrypt(nil, rec1); err != nil {
		t.Fatal(err)
	}
	if _, err := pr.decrypt(nil, rec2); err != nil {
		t.Fatal(err)
	}

	// The long messages are pinned by their SHA-256; the short ones whole.
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	got := map[string]string{
		"hostProof": hex.EncodeToString(hostProof), "phoneProof": hex.EncodeToString(phoneProof),
		"msg1": sum(msg1), "msg2": sum(msg2), "msg3": hex.EncodeToString(msg3),
		"rec1": hex.EncodeToString(rec1), "rec2": hex.EncodeToString(rec2),
	}
	for name, v := range got {
		if want := knownAnswer[name]; v != want {
			t.Errorf("%s = %s\nwant %s", name, v, want)
		}
	}
}

// knownKEMSecret and knownCiphertext are one ML-KEM-1024 encapsulation to the
// key seeded with 64 bytes of 0x05, captured once with its secret.
const knownKEMSecret = "51cc32be6ea7193e5b25462243ff24efed1de151ca0b687d6fbd572406305e5f"

const knownCiphertext = "" +
	"0fc1ba3844b16c73c7e900e7eb693daf540bca90ba36d47577ce9137f185d5ceb5931f5553374b93b2054eaa267d7dd34aa26acf3bdd7f39b973aff08867b0b5" +
	"11d28fb12b4c9b48b26c4bce0f297b529a9b3bfb14bbfd5c28cfdfb031ccba96b3c9da55950385a435e1596d007132d0be151f7803d58b6e83f9b0bb72fcdfe2" +
	"734f74a0e85103a408189910e08cf4276aa2088322209696279c173676458167d8f8ee9ac4de63c14a2442df5b34df88614727df1b8d89de32d57b38f1f02687" +
	"9b4f5c659d1f2511145017a7649d4dcaaea398937828be6c77a0f143e3d6de03e65d02986eec52ecb9e7b70da646c5318f5119440df699f505f50b2bb5443828" +
	"4a760da1d709e8f75e205a7e42464d27c8591480d5a6a8bee580c54d3ab4adb2dff09ecf601e7ba6cce6c4f08fe52f554a5a2d2c5fefe764530ba70df1b7b457" +
	"a8c16841997e079faa4f2531326a9a1214a7263425ad985afc1cfcfcca57221d7ab03b84d22a11282375ac331b2bd17d0d4b06e3aefa0860b654420fcfa4afcf" +
	"60ec046c7f4e133e5af7d957be1d0d3ab127e74cb5eeedb9c6d770c59ff275f77b9c9b4439cfff9d8fd705dd21ea47995028ef07c9a038e4fb8a288bbc1c89de" +
	"6c8336ca51660420303f4bd107be98958aa3eb2cdcf5879cf8b5bae898f2aca5106b1e4af7d38ec4646223167940bac7ec55540bf9f19f7cc58355bc13d685b5" +
	"373e87f651166818a058a1faf3d4526aa88b76f2a1ba6327134df54e8e8679bed3799e5c21889caec8b122c10eaba7d77516cf006f0d6d55d8d2c993577897a0" +
	"4ca006d0eee54025f0ff326ccc8fa2ed0a7a6fed2ac01e45f4c1f9f13e4d9d59c0830c9e44bc97c798a829935598aab0eb8f3683c7dc9f6da008249c84de0d46" +
	"a240c68d535c3e008f1b3f7ee339538ab2f19bd2ae15475294b771dcd04fd310044e3eff8a23a79056659afa0fb70da7fb480cff75085609dc6c7092a350e4f5" +
	"4a838efe8ab1df65d51d3989e249949d0eb3577fdbd3331c3b246f08600cea30ac5dfde03cb2d23feb5fbaaf105699abae15079dd379c8c1862368ac417e88d1" +
	"d2c5759924a9812b0c0608423877fab29e78f92c388287ce4f2fcf780554692f7ed4088c1d3213acf0879adda8b14df10e37d89da1889ba1aa97cfe747d871c5" +
	"5fd48cd49890e71a19b9c781b2fd05b549c7a21f7a2a1c131de586831e8da75005bf20448b32eb113cfb9591b0f0e9142119702f4b149132ef3c9fafd0cfcfc7" +
	"d7b6ae5b8c0fb74444b1988333c77a6dc1a5818eb5eda5bf84bb9c119585063c33443a69a0a0e9b13dd1637ec017c8071caaad204443bd61030b4fd1153d18b5" +
	"76141ae69417f36df96452222f6aa74596c3c070ecc658aaf15923f237ebdc93a2c8b9aa5e8acf4a69aa88879fd1e9c5cd92e8ab82c575ab2d3a2b7d70d80d1d" +
	"75eae634f92aecaf2b994fc84c5ffcce7fecdeac9e5fe5720cd7fd79a22f8754f728ab012406d69d3866b48e15634c11b640fe5d21ac73a72d32491b864a287d" +
	"0db1e5d66a614611bc9103e9c05359958105faef2a1091b885bbe503c90129ed1f76cd2ad2af1d3df47fb074848ae14208c3af571ad728b9923f6875c2c89ad4" +
	"230ae1a7ae6301f76a633316f0510ab042ef7a37d1b5011743855a288dbd947340aa2a866c1a3a2bfd3dbc74c3d685bd1ade39abe3e5f06bea7d11c942de4930" +
	"72409670820f60edec96e74eb8be80ed8f54d7c6a9813f3778b0ac9e978fe9d5a9ffe034463951a85af1dd209df91dab2ecba32f806cab7df1250e86645afed0" +
	"d1f35041f2036f037d482b59a63e39792a0364d825ed9997041c31bb93aa1c3967d117a29333c244cf5707eefdcba42b1c4418e900f461ca159c4f177190e771" +
	"e4fbcdd2c7771577e02710feef3efd77255c83361ea57c65407b2eccc5a0d873171eb00ffff6237dae69bff5b1ee7237740564cde60ac391c463fa2fd8968ff2" +
	"7d2124714909515ef48ea3cf93a7be05806f0cc90c997906a128bc6e18bcfa156c644fe539a2ba63014ca37a744c8e4933d3d7d19f2b077849c7190a726fa68d" +
	"aaee44d1edcff3db64d784a1e33f8e64aae3f8c89f9d80552b0aabd596145f93f58dd9d9e66e79c925039bbec208c54b82ab2148f2a041427eccf79928d164b1" +
	"dfa5183ba7e9bbd7379ac71fb56a7685ecfeaa26324a11e9a0d8dabd55ac4992"

var knownAnswer = map[string]string{
	"hostProof":  "a09aa5f47a6759802ff955f8dc2d2a14a5c99d23be97f864127ff9383455a4f0225d558ccb4907e35dc85e6af0615399ad68e42bd58dd9a846da8358a21846ddaf1b589c47193e92a1c96cc93692e67a6e3e1f149f9488e08f539026bbebdc0a",
	"phoneProof": "d04ab232742bb4ab3a1368bd4615e4e6d0224ab71a016baf8520a332c9778737db8ed25b101429d3bbf5a5a85b6d995a7d15e863f8ff0723e2f594106088618f308730dc554b2610156c75acc6bda9b44c68ad7246aa9dc2c95eb7c2a8600405",
	"msg1":       "63a18e31b21e31a716b23029bf707e2b1ae7c3a654e4ed2f552f41e8306f85df",
	"msg2":       "1f4027b771227fbabcc67b982c0ac8d5ec3a41f161f7780021146993a6f1f2e9",
	"msg3":       "59dfe8a0a5282f395bc23c276ed1af4e6cc218c0ac35c4260fec58186d0b6ed7ae21123d310c1112781ff7ff119f6768f65515c8ba6a5942fbb584bf13708972b82a433cfcb0477eb1cb92d66935546f9960823de7336009cc9653f56941cfb8a2a4c0ee813b20cf9d92f34da91eff4b40b241b63696e5fedd8598af6b338fdd7282c9a5c5e3097a6cd75c23542cc1fc4031312158258785d56e547fd7e1a716",
	"rec1":       "9fb9f20b18af086853c756a7cf2272bf0e03203033bd7a75738945ea",
	"rec2":       "5c833ca1b25b6ee4f3f4337ec74f11a258a65b223a916924e0a3865e",
}

// A message that will not compress grows by exactly RecordOverhead, which the
// relay's frame limit counts on.
func TestRecordOverheadIsExact(t *testing.T) {
	phone, host := identities(t)
	ps, _, err := pipe(t, phone, host, host.Public, phone.ID)
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, jsonrpc.MaxMessageBytes)
	rand.Read(plain)
	if got := len(seal(t, ps, plain)); got != len(plain)+RecordOverhead {
		t.Fatalf("sealed %d bytes into %d, want %d", len(plain), got, len(plain)+RecordOverhead)
	}
}
