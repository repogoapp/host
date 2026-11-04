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

// The same vector as HostTransport.rotateMessage's Swift test.
func TestRotateMessageVector(t *testing.T) {
	newPublic := []byte{4}
	for i := 1; i <= 64; i++ {
		newPublic = append(newPublic, byte(i))
	}
	got, err := RotateMessage("26b1b9b47da2817dca94c31be4ae2387", "17597de8e726e7d763bf8b2ae179193b", newPublic)
	if err != nil {
		t.Fatal(err)
	}
	want := "7265706f676f2d726f746174652d763126b1b9b47da2817dca94c31be4ae238717597de8e726e7d763bf8b2ae179193b41040102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40"
	if hex.EncodeToString(got) != want {
		t.Fatalf("RotateMessage = %x", got)
	}
}
