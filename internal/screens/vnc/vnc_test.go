package vnc

import (
	"crypto/aes"
	"crypto/md5"
	"math/big"
	"testing"
)

func TestCopyRectMovesPixels(t *testing.T) {
	c := &Client{}
	c.resize(4, 2)
	copy(c.Screen[0:4], []byte{1, 2, 3, 4}) // pixel 0,0
	c.copyRect(0, 0, 3, 1, 1, 1)
	at := (1*4 + 3) * 4
	if string(c.Screen[at:at+4]) != string([]byte{1, 2, 3, 4}) {
		t.Fatalf("pixel at 3,1 = %v", c.Screen[at:at+4])
	}
}

// The server can decrypt what AppleCredentials sends: it derives the same key
// from its own private key and the client's public one.
func TestAppleCredentialsReachTheServer(t *testing.T) {
	p, _ := new(big.Int).SetString("ffffffffffffffffc90fdaa22168c234c4c6628b80dc1cd129024e088a67cc74020bbea63b139b22514a08798e3404ddef9519b3cd3a431b302b0a6df25f14374fe1356d6d51c245e485b576625e7ec6f44c42e9a63a3620ffffffffffffffff", 16)
	prime := p.FillBytes(make([]byte, 96))
	serverPrivate := big.NewInt(123456789)
	serverKey := new(big.Int).Exp(big.NewInt(2), serverPrivate, p).FillBytes(make([]byte, 96))
	credentials, clientKey, err := AppleCredentials(2, prime, serverKey, "server", "pass")
	if err != nil || len(credentials) != 128 || len(clientKey) != 96 {
		t.Fatalf("%d %d %v", len(credentials), len(clientKey), err)
	}
	shared := new(big.Int).Exp(new(big.Int).SetBytes(clientKey), serverPrivate, p)
	key := md5.Sum(shared.FillBytes(make([]byte, 96)))
	block, _ := aes.NewCipher(key[:])
	plain := make([]byte, 128)
	for i := 0; i < 128; i += 16 {
		block.Decrypt(plain[i:i+16], credentials[i:i+16])
	}
	if string(plain[:7]) != "server\x00" || string(plain[64:69]) != "pass\x00" {
		t.Fatalf("decrypted %q / %q", plain[:7], plain[64:69])
	}
}
