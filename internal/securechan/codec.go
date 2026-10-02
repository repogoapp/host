package securechan

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"

	"github.com/repogo/host/internal/jsonrpc"
)

// Body codecs, the first byte of every record's plaintext; SecureChannel.BodyCodec
// in Swift mirrors them. Each message is compressed alone, so no length leaks another's content.
const (
	codecRaw     byte = 0x00
	codecDeflate byte = 0x01
)

const (
	// Below this, deflate saves a few hundred bytes at most (bench/compress).
	compressMin = 1 << 10
	// A compressed body is kept only when it is at most 90% of the original:
	// already-compressed data (images, archives) goes raw.
	compressKeepPercent = 90
)

var (
	errCodec     = errors.New("securechan: unknown body codec; the peer runs an older build")
	errInflate   = errors.New("securechan: compressed body is corrupt")
	errOversized = errors.New("securechan: decompressed body exceeds the message limit")
)

// encodeBody prefixes plain with its codec, deflating it when that pays.
func encodeBody(plain []byte) []byte {
	if len(plain) >= compressMin {
		var buf bytes.Buffer
		buf.WriteByte(codecDeflate)
		w, _ := flate.NewWriter(&buf, flate.BestSpeed)
		w.Write(plain)
		w.Close()
		if (buf.Len()-1)*100 <= len(plain)*compressKeepPercent {
			return buf.Bytes()
		}
	}
	return append([]byte{codecRaw}, plain...)
}

// decodeBody reverses encodeBody. The output is capped at the JSON-RPC message
// limit, so a body cannot inflate past what an uncompressed one could carry.
func decodeBody(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return nil, errCodec
	}
	switch body[0] {
	case codecRaw:
		return body[1:], nil
	case codecDeflate:
		r := flate.NewReader(bytes.NewReader(body[1:]))
		defer r.Close()
		plain, err := io.ReadAll(io.LimitReader(r, jsonrpc.MaxMessageBytes+1))
		if err != nil {
			return nil, errInflate
		}
		if len(plain) > jsonrpc.MaxMessageBytes {
			return nil, errOversized
		}
		return plain, nil
	default:
		return nil, errCodec
	}
}
