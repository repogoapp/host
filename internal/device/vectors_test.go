package device

import (
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// A signature CryptoKit made with P256.Signing (the Secure Enclave key's
// algorithm): what the iPhone sends must verify here byte for byte.
func TestACryptoKitP256SignatureVerifies(t *testing.T) {
	pub, _ := base64.StdEncoding.DecodeString("BCpOykA60Q/FKKgzbjQPWvOJSt83IZfgnslm7PhUH20FI6UoTHytS7L7CCjyBCaGoSosOe9rTqRPiUH4HnqNYTU=")
	sig, _ := base64.StdEncoding.DecodeString("PyVOl3QQ5uLLc+WfM3jU6/8MTA9TQQ8FoIbSiazEQxdzm/jYxMwcwjkel7xpzi9/IGAuCEz045yB1JAO09NcwA==")
	if !VerifyPhone(pub, []byte("repogo-p256-vector"), sig) {
		t.Fatal("a CryptoKit P-256 signature did not verify")
	}
}

// The same vector as PairingClient.pairMessage's Swift test.
func TestPairMessageVector(t *testing.T) {
	joiner := []byte{4}
	for i := 1; i <= 64; i++ {
		joiner = append(joiner, byte(i))
	}
	spki := make([]byte, 32)
	for i := range spki {
		spki[i] = 7
	}
	got, err := PairMessage("26b1b9b47da2817dca94c31be4ae2387", joiner, SignedPin{SPKI: spki, Counter: 1762300000})
	if err != nil {
		t.Fatal(err)
	}
	want := "7265706f676f2d706169722d686f73742d763126b1b9b47da2817dca94c31be4ae23870041040102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40070707070707070707070707070707070707070707070707070707070707070700000000690a9060"
	if hex.EncodeToString(got) != want {
		t.Fatalf("PairMessage = %x", got)
	}
}
