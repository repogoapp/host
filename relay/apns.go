package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/sideshow/apns2"
	"github.com/sideshow/apns2/token"

	"github.com/repogo/host/internal/relay"
)

// apnsPush delivers push.send with the APNs key from the environment; nil with
// no key, so a dev relay answers Unavailable. The phone says which of sandbox
// and production minted its token.
func apnsPush(log *slog.Logger) func(context.Context, relay.PushRequest) error {
	key := os.Getenv("APNS_KEY")
	if key == "" {
		return nil
	}
	authKey, err := token.AuthKeyFromBytes([]byte(key))
	if err != nil {
		log.Error("apns: unreadable key", "err", err)
		return nil
	}
	tok := &token.Token{AuthKey: authKey, KeyID: os.Getenv("APNS_KEY_ID"), TeamID: os.Getenv("APNS_TEAM_ID")}
	topic := env("APNS_TOPIC", "app.repogo")
	// The relay refuses any other environment before calling this.
	clients := map[string]*apns2.Client{
		relay.EnvironmentSandbox:    apns2.NewTokenClient(tok).Development(),
		relay.EnvironmentProduction: apns2.NewTokenClient(tok).Production(),
	}
	return func(ctx context.Context, req relay.PushRequest) error {
		client := clients[req.Environment]
		note := &apns2.Notification{
			DeviceToken: req.Token,
			Topic:       topic,
			CollapseID:  req.CollapseID,
			PushType:    apns2.PushTypeAlert,
			Priority:    req.Priority,
			Payload:     []byte(req.Payload),
		}
		if req.PushType == relay.PushTypeLiveActivity {
			// Apple routes Live Activity pushes by topic, and they carry no collapse id.
			note.Topic = topic + ".push-type.liveactivity"
			note.PushType = apns2.PushTypeLiveActivity
			note.CollapseID = ""
		}
		res, err := client.PushWithContext(ctx, note)
		if err != nil {
			return err
		}
		switch {
		case res.Sent():
			return nil
		case res.Reason == apns2.ReasonUnregistered, res.Reason == apns2.ReasonBadDeviceToken:
			return relay.ErrUnregistered
		default:
			return fmt.Errorf("apns: %d %s", res.StatusCode, res.Reason)
		}
	}
}
