package securechan

import (
	"crypto/sha256"

	"github.com/repogoapp/host/internal/device"
)

// PairingRejection binds the refusal to this phone's fresh handshake so a
// relay cannot forge it or replay a refusal after the phone pairs again.
func PairingRejection(identity *device.Identity, peer device.ID, request []byte) ([]byte, error) {
	if len(request) != Message1Len || request[0] != PairedHandshake {
		return nil, errLength
	}
	phone, err := peer.Bytes()
	if err != nil {
		return nil, err
	}
	host, err := identity.ID.Bytes()
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(request)
	message := append([]byte("repogo-not-paired-v1"), phone...)
	message = append(message, host...)
	message = append(message, digest[:]...)
	return append([]byte{NotPaired}, identity.Sign(message)...), nil
}
