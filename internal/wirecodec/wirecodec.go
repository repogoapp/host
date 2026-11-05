// Package wirecodec compresses each JSON-RPC message on a socket whose hello
// agreed to it: one codec byte, then the body. Each message is compressed
// alone, so no length leaks another's content. Swift's WireCodec mirrors it.
package wirecodec

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"

	"github.com/repogoapp/host/internal/jsonrpc"
)

// The codec byte that opens every message.
const (
	Raw     byte = 0x00
	Deflate byte = 0x01
)

// Overhead is what framing adds to a message at most: its codec byte.
const Overhead = 1

const (
	// Below this, deflate saves a few hundred bytes at most (bench/compress).
	compressMin = 1 << 10
	// A compressed body is kept only when it is at most 90% of the original:
	// already-compressed data (images, archives) goes raw.
	compressKeepPercent = 90
)

var (
	ErrCodec     = errors.New("wirecodec: unknown codec byte")
	ErrInflate   = errors.New("wirecodec: compressed message is corrupt")
	ErrOversized = errors.New("wirecodec: decompressed message exceeds the message limit")
)

// Encode prefixes plain with its codec, deflating it when that pays.
func Encode(plain []byte) []byte {
	if len(plain) >= compressMin {
		var buf bytes.Buffer
		buf.WriteByte(Deflate)
		w, _ := flate.NewWriter(&buf, flate.BestSpeed)
		w.Write(plain)
		w.Close()
		if (buf.Len()-1)*100 <= len(plain)*compressKeepPercent {
			return buf.Bytes()
		}
	}
	return append([]byte{Raw}, plain...)
}

// Decode reverses Encode. The output is capped at the JSON-RPC message limit,
// so a message cannot inflate past what an uncompressed one could carry.
func Decode(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return nil, ErrCodec
	}
	switch body[0] {
	case Raw:
		return body[1:], nil
	case Deflate:
		r := flate.NewReader(bytes.NewReader(body[1:]))
		defer r.Close()
		plain, err := io.ReadAll(io.LimitReader(r, jsonrpc.MaxMessageBytes+1))
		if err != nil {
			return nil, ErrInflate
		}
		if len(plain) > jsonrpc.MaxMessageBytes {
			return nil, ErrOversized
		}
		return plain, nil
	default:
		return nil, ErrCodec
	}
}
