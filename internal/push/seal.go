package push

import (
	"encoding/json"
	"strconv"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/pushseal"
)

// SealKey is the secret peer's Live Activity pushes are sealed with; nil
// means the phone gets none, since nothing goes out in the clear.
func SealKey(peer device.Peer) []byte { return peer.SealKey(pushseal.FeatureLiveActivities) }

// AlertKey is the secret peer's alerts are sealed with; nil means it gets
// none. An Android app opens alerts as it opens Live Updates; an iPhone needs
// its notification service extension (pushseal.FeatureAlerts).
func AlertKey(peer device.Peer) []byte {
	if IsFCM(peer.Push) {
		return SealKey(peer)
	}
	return peer.SealKey(pushseal.FeatureAlerts)
}

// Sealed is an iOS content-state or attributes value that carries only the box.
type Sealed struct {
	Sealed pushseal.Envelope `json:"sealed"`
}

// SealData seals an FCM data map (Data's output) whole, for an Android phone
// with a push key: the data FCM sees is the host and the box.
func SealData(key []byte, host string, data []byte) ([]byte, error) {
	env, err := pushseal.Seal(key, host, pushseal.FCM, "", json.RawMessage(data))
	if err != nil {
		return nil, err
	}
	return Data(map[string]any{"host_id": host, "v": strconv.Itoa(env.V), "kid": env.KID, "box": env.Box}), nil
}
