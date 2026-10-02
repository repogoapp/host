// Command pq measures what a hybrid post-quantum handshake would add to the
// channel: today's full Noise XX handshake through internal/securechan, then
// the ML-KEM operations an XXhfs handshake runs on top of it.
//
//	go run ./bench/pq
package main

import (
	"context"
	"crypto/mlkem"
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/securechan"
)

func main() {
	ctx := context.Background()
	phone, err := device.Generate()
	if err != nil {
		fail(err)
	}
	host, err := device.Generate()
	if err != nil {
		fail(err)
	}

	fmt.Printf("%s/%s %s\n\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Printf("%-44s %12s\n", "operation", "µs/op")
	report("handshake today (both sides, XX)", func(b *testing.B) {
		for range b.N {
			if err := handshake(ctx, phone, host); err != nil {
				b.Fatal(err)
			}
		}
	})

	// XXhfs: the phone makes a KEM key pair and sends the public key in
	// message 1; the host encapsulates to it in message 2; the phone decapsulates.
	report("ML-KEM-768 key pair (phone)", func(b *testing.B) {
		for range b.N {
			if _, err := mlkem.GenerateKey768(); err != nil {
				b.Fatal(err)
			}
		}
	})
	dk768, _ := mlkem.GenerateKey768()
	report("ML-KEM-768 encapsulate (host)", func(b *testing.B) {
		for range b.N {
			dk768.EncapsulationKey().Encapsulate()
		}
	})
	_, ct768 := dk768.EncapsulationKey().Encapsulate()
	report("ML-KEM-768 decapsulate (phone)", func(b *testing.B) {
		for range b.N {
			if _, err := dk768.Decapsulate(ct768); err != nil {
				b.Fatal(err)
			}
		}
	})
	report("ML-KEM-1024 key pair (phone)", func(b *testing.B) {
		for range b.N {
			if _, err := mlkem.GenerateKey1024(); err != nil {
				b.Fatal(err)
			}
		}
	})
	dk1024, _ := mlkem.GenerateKey1024()
	report("ML-KEM-1024 encapsulate (host)", func(b *testing.B) {
		for range b.N {
			dk1024.EncapsulationKey().Encapsulate()
		}
	})
	_, ct1024 := dk1024.EncapsulationKey().Encapsulate()
	report("ML-KEM-1024 decapsulate (phone)", func(b *testing.B) {
		for range b.N {
			if _, err := dk1024.Decapsulate(ct1024); err != nil {
				b.Fatal(err)
			}
		}
	})

	fmt.Printf("\nbytes added per handshake: ML-KEM-768 %d (key %d + ciphertext %d + tag 16), ML-KEM-1024 %d\n",
		mlkem.EncapsulationKeySize768+mlkem.CiphertextSize768+16, mlkem.EncapsulationKeySize768, mlkem.CiphertextSize768,
		mlkem.EncapsulationKeySize1024+mlkem.CiphertextSize1024+16)
}

// handshake runs both sides in one goroutine: the responder answers inside
// the initiator's send, so the three messages pass in order.
func handshake(ctx context.Context, phone, host *device.Identity) error {
	var responder *securechan.Responder
	var inbox []byte
	send := func(_ context.Context, msg []byte) error {
		if responder == nil {
			r, msg2, err := securechan.Respond(host, phone.ID, msg)
			responder, inbox = r, msg2
			return err
		}
		_, err := responder.Finish(msg)
		return err
	}
	recv := func(context.Context) ([]byte, error) { return inbox, nil }
	_, err := securechan.Initiate(ctx, phone, host.ID, host.Public, send, recv)
	return err
}

func report(name string, body func(b *testing.B)) {
	r := testing.Benchmark(body)
	fmt.Printf("%-44s %12.1f\n", name, float64(r.T.Nanoseconds())/float64(r.N)/1e3)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "pq:", err)
	os.Exit(1)
}
