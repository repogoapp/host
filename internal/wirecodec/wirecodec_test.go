package wirecodec

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/jsonrpc"
)

// vectorJSON is the message both cross-language vectors compress; the Swift
// test builds the same bytes.
func vectorJSON() []byte {
	events := make([]string, 24)
	for i := range events {
		events[i] = fmt.Sprintf(`{"idx":%d,"kind":"assistant","turn":"t_1","text":"retry the upload"}`, i)
	}
	return []byte(`{"jsonrpc":"2.0","id":1,"result":{"events":[` + strings.Join(events, ",") + `]}}`)
}

// appleDeflate is vectorJSON compressed by Apple's Compression framework
// (NSData.compressed(using: .zlib)), what an iPhone sends.
const appleDeflate = "add25b0ac2301085e1ad94f31ca413efd94a11293660b5a425994aa564ef4670079ec7c964fea76fc5238d214e3738d84d0d83be831383e8d33c28dc0afff241135cb396dd02571b3cfb507ea14da94fda062d673ac7509ef42adfc12fe5b43434be2bbdfb6a9e86b1ed90cdaf2184862534b684c68ed0d8131a0742e348689c088d33a12114a80ca9c2a02a0cabc2c02a0cadc2e02a0cafc2002b0cb19621d632c45a8658fb8fd84bce1f"

func TestEncodePicksBySize(t *testing.T) {
	random := make([]byte, 4<<10)
	rand.Read(random)
	for _, tc := range []struct {
		name  string
		plain []byte
		codec byte
	}{
		{"empty", nil, Raw},
		{"small", []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), Raw},
		{"large json", vectorJSON(), Deflate},
		{"incompressible", random, Raw},
	} {
		body := Encode(tc.plain)
		if body[0] != tc.codec {
			t.Errorf("%s: codec %#x, want %#x", tc.name, body[0], tc.codec)
		}
		got, err := Decode(body)
		if err != nil || !bytes.Equal(got, tc.plain) {
			t.Errorf("%s: round trip failed: %v", tc.name, err)
		}
	}
}

func TestEncodeShrinksJSON(t *testing.T) {
	plain := vectorJSON()
	if body := Encode(plain); len(body)*4 > len(plain) {
		t.Fatalf("%d bytes compressed to %d; want at least 4x", len(plain), len(body))
	}
}

func TestDecodesApple(t *testing.T) {
	packed, err := hex.DecodeString(appleDeflate)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(append([]byte{Deflate}, packed...))
	if err != nil || !bytes.Equal(got, vectorJSON()) {
		t.Fatalf("Apple deflate did not decode to the vector: %v", err)
	}
}

func TestDecodeRejects(t *testing.T) {
	var bomb bytes.Buffer
	w, _ := flate.NewWriter(&bomb, flate.BestSpeed)
	w.Write(make([]byte, jsonrpc.MaxMessageBytes+1))
	w.Close()
	for _, tc := range []struct {
		name string
		body []byte
		want error
	}{
		{"empty", nil, ErrCodec},
		{"bare JSON", []byte(`{"jsonrpc":"2.0"}`), ErrCodec},
		{"corrupt deflate", []byte{Deflate, 0xff, 0xff, 0xff}, ErrInflate},
		{"inflates past the limit", append([]byte{Deflate}, bomb.Bytes()...), ErrOversized},
	} {
		if _, err := Decode(tc.body); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}
