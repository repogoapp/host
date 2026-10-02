// Package relay moves opaque bytes between a user's devices, keeping only who
// is attached and who talked to whom.
package relay

import (
	"errors"

	"github.com/repogo/host/internal/device"
)

// Envelope framing: [version:1][target device id:16][payload:...]. A fixed
// header so routing never decodes the payload.
const (
	envelopeVersion = 1
	headerLen       = 1 + device.IDLen
)

var (
	errShortMessage = errors.New("relay: message shorter than its header")
	errVersion      = errors.New("relay: unsupported envelope version")
)

// Encode prefixes a payload with its destination.
func Encode(to device.ID, payload []byte) ([]byte, error) {
	target, err := to.Bytes()
	if err != nil {
		return nil, err
	}
	out := make([]byte, headerLen+len(payload))
	out[0] = envelopeVersion
	copy(out[1:headerLen], target)
	copy(out[headerLen:], payload)
	return out, nil
}

// Decode splits a message into its destination and its opaque body.
func Decode(msg []byte) (device.ID, []byte, error) {
	if len(msg) < headerLen {
		return "", nil, errShortMessage
	}
	if msg[0] != envelopeVersion {
		return "", nil, errVersion
	}
	return device.IDFromBytes(msg[1:headerLen]), msg[headerLen:], nil
}
