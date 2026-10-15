package tunnelwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	for _, m := range []Message{
		&Hello{PublicKey: []byte{1, 2, 3}},
		&Challenge{Nonce: bytes.Repeat([]byte{7}, 32), GatewayID: "tunnels.repogo.dev", Time: 1800000000000},
		&Proof{Signature: []byte{9, 9}},
		&Ready{},
		&Request{SessionID: "s1", Method: "POST", Path: "/a?b=1", Target: "3000", HeaderLines: []string{"Host: x.tunnels.repogo.dev"}, Body: []byte("payload")},
		&Response{SessionID: "s1", Status: 200, HeaderLines: []string{"Content-Type: text/plain"}, Body: []byte{0, 255, 10}},
		&Response{SessionID: "s2", Status: 200, HeaderLines: []string{}, Streaming: true, Body: []byte{}},
		&BodyChunk{SessionID: "s2", EndOfStream: true, Data: []byte("tail")},
		&WSOpen{SessionID: "w1", Target: "5173", Path: "/hmr", HeaderLines: []string{"Origin: https://x"}, Subprotocols: []string{"vite-hmr"}},
		&WSOpenAck{SessionID: "w1", Subprotocol: "vite-hmr"},
		&WSFrame{SessionID: "w1", Binary: true, Data: []byte{1, 2}},
		&WSClose{SessionID: "w1", Code: 1000, Reason: "bye"},
	} {
		b, err := Encode(m)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(b)
		if err != nil {
			t.Fatalf("%T: %v", m, err)
		}
		if !reflect.DeepEqual(got, m) {
			t.Errorf("%T: got %+v, want %+v", m, got, m)
		}
	}
}

// Bodies cross as raw bytes, so a megabyte costs a megabyte.
func TestBodyIsNotEncoded(t *testing.T) {
	body := bytes.Repeat([]byte{0xff}, 1<<20)
	b, err := Encode(&BodyChunk{SessionID: "s1", Data: body})
	if err != nil {
		t.Fatal(err)
	}
	if overhead := len(b) - len(body); overhead > 64 {
		t.Fatalf("frame adds %d bytes to a 1 MB body", overhead)
	}
}

func TestDecodeRefusesMalformedFrames(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":           {},
		"short":           {byte(KindReady), 0, 0},
		"header overruns": {byte(KindReady), 0, 0, 0, 9, '{', '}'},
		"unknown kind":    {200, 0, 0, 0, 2, '{', '}'},
		"bad header":      {byte(KindRequest), 0, 0, 0, 1, '['},
	} {
		if _, err := Decode(b); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}
