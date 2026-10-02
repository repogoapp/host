// Command aead compares the channel's cipher, AES-256-GCM, with
// ChaCha20-Poly1305 on this machine: one seal and one open per record, with
// each cipher's Noise counter nonce, as internal/securechan's cipherState does.
//
//	go run ./bench/aead
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"runtime"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

// Record sizes from a short chat delta up to a large transcript page.
var sizes = []int{256, 4 << 10, 64 << 10, 1 << 20}

func main() {
	flag.Parse()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		fail(err)
	}
	chacha, err := chacha20poly1305.New(key)
	if err != nil {
		fail(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		fail(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		fail(err)
	}

	fmt.Printf("%s/%s %s\n\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Printf("%-10s %-20s %12s %12s\n", "size", "cipher", "ns/record", "MB/s")
	for _, size := range sizes {
		for _, c := range []struct {
			name  string
			aead  cipher.AEAD
			order binary.ByteOrder
		}{
			// Noise §12.3: ChaChaPoly puts the counter little-endian; §12.4: AESGCM big-endian.
			{"chacha20-poly1305", chacha, binary.LittleEndian},
			{"aes-256-gcm", gcm, binary.BigEndian},
		} {
			r := testing.Benchmark(func(b *testing.B) { roundTrip(b, c.aead, c.order, size) })
			nsPerRecord := float64(r.T.Nanoseconds()) / float64(r.N)
			mbPerSec := float64(size) / nsPerRecord * 1e3
			fmt.Printf("%-10s %-20s %12.0f %12.0f\n", label(size), c.name, nsPerRecord, mbPerSec)
		}
	}
}

// roundTrip seals and opens one record per iteration, allocating the output
// as cipherState does, so the numbers include the copies the host makes.
func roundTrip(b *testing.B, aead cipher.AEAD, order binary.ByteOrder, size int) {
	plain := make([]byte, size)
	if _, err := rand.Read(plain); err != nil {
		b.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		order.PutUint64(nonce[4:], uint64(i))
		sealed := aead.Seal(nil, nonce, plain, nil)
		if _, err := aead.Open(nil, nonce, sealed, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func label(size int) string {
	if size >= 1<<20 {
		return fmt.Sprintf("%d MB", size>>20)
	}
	if size >= 1<<10 {
		return fmt.Sprintf("%d KB", size>>10)
	}
	return fmt.Sprintf("%d B", size)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "aead:", err)
	os.Exit(1)
}
