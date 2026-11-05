package pushgate

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/sideshow/apns2"
	"github.com/sideshow/apns2/token"
)

// APNs delivers with the key in APNS_AUTH_KEY, APNS_KEY_ID and APNS_TEAM_ID,
// to APPLE_BUNDLE_ID's topics; nil with no key, so a dev relay refuses pushes.
// The phone says which of sandbox and production minted its token.
func APNs(log *slog.Logger) Deliver {
	key := os.Getenv("APNS_AUTH_KEY")
	if key == "" {
		return nil
	}
	authKey, err := token.AuthKeyFromBytes([]byte(key))
	if err != nil {
		log.Error("apns: unreadable key", "err", err)
		return nil
	}
	tok := &token.Token{AuthKey: authKey, KeyID: os.Getenv("APNS_KEY_ID"), TeamID: os.Getenv("APNS_TEAM_ID")}
	topic := BundleID()
	// The gate refuses any other environment before calling this.
	clients := map[string]*apns2.Client{
		EnvironmentSandbox:    apns2.NewTokenClient(tok).Development(),
		EnvironmentProduction: apns2.NewTokenClient(tok).Production(),
	}
	return func(ctx context.Context, req PushRequest) error {
		note := &apns2.Notification{
			DeviceToken: req.Token,
			Topic:       topic,
			CollapseID:  req.CollapseID,
			PushType:    apns2.PushTypeAlert,
			Priority:    req.Priority,
			Payload:     []byte(req.Payload),
		}
		if req.PushType == PushTypeLiveActivity {
			// Apple routes Live Activity pushes by topic, and they carry no collapse id.
			note.Topic = topic + ".push-type.liveactivity"
			note.PushType = apns2.PushTypeLiveActivity
			note.CollapseID = ""
		}
		res, err := clients[req.Environment].PushWithContext(ctx, note)
		if err != nil {
			return err
		}
		switch {
		case res.Sent():
			return nil
		case res.Reason == apns2.ReasonUnregistered, res.Reason == apns2.ReasonBadDeviceToken:
			// The reason travels to the host, which logs why it forgot the token.
			return fmt.Errorf("%w: %d %s", ErrUnregistered, res.StatusCode, res.Reason)
		default:
			return fmt.Errorf("apns: %d %s", res.StatusCode, res.Reason)
		}
	}
}

// BundleID is the app the pushes go to.
func BundleID() string { return cmp.Or(os.Getenv("APPLE_BUNDLE_ID"), "app.repogo") }

// AppID is the App Attest app id: the team prefix and the bundle.
func AppID() string {
	return cmp.Or(os.Getenv("APPLE_APP_ID_PREFIX"), os.Getenv("APNS_TEAM_ID")) + "." + BundleID()
}
