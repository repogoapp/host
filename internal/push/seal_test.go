package push

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/push/pushtest"
	"github.com/repogoapp/host/internal/pushgate"
	"github.com/repogoapp/host/internal/pushseal"
)

// An Android phone and an iPhone with keys both get the alert sealed.
func TestAndroidAlertIsSealed(t *testing.T) {
	store, _ := storeWithPhones(t)
	android, _ := device.Generate()
	if err := store.Add(device.Peer{ID: android.ID, Public: android.Public}); err != nil {
		t.Fatal(err)
	}
	fcm := android.GrantPush(store.Identity().ID, device.PushTarget{Service: device.PushFCM, Token: "fJ3x:APA91b"})
	if err := store.RegisterPush(android.ID, "", fcm); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{3}, pushseal.SecretSize)
	if err := store.SetClient(android.ID, device.Client{Platform: "android", Features: map[string]int{device.FeaturePushSeal: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetPushKey(android.ID, key); err != nil {
		t.Fatal(err)
	}
	sender := &pushtest.Sender{}
	n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.NeedsYouAlert(context.Background(), NeedsYou{ChatID: "claude:s1", Agent: "claude", Title: "Approval needed", Ask: "Bash: npm test"})

	host := string(store.Identity().ID)
	for _, call := range sender.Calls {
		if call.Service != device.PushFCM {
			if strings.Contains(string(call.Payload), "Approval needed") {
				t.Errorf("iOS alert in the clear: %s", call.Payload)
			}
			continue
		}
		if strings.Contains(string(call.Payload), "Approval") || strings.Contains(string(call.Payload), "claude:s1") {
			t.Fatalf("alert in the clear: %s", call.Payload)
		}
		var data map[string]string
		_ = json.Unmarshal(call.Payload, &data)
		plain, err := pushseal.Open(key, host, pushseal.FCM, "", pushseal.Envelope{V: 1, KID: data["kid"], Box: data["box"]})
		if err != nil {
			t.Fatal(err)
		}
		var inner map[string]string
		_ = json.Unmarshal(plain, &inner)
		if inner["kind"] != "alert" || inner["title"] != "Approval needed" || inner["chat_id"] != "claude:s1" {
			t.Fatalf("opened %v", inner)
		}
		return
	}
	t.Fatal("no android push")
}

// An iPhone whose extension opens alerts gets fixed text, mutable-content and
// the box; one that opens only Live Activities gets no alerts at all.
func TestIOSAlertIsSealedForTheExtension(t *testing.T) {
	store, iphone := storeWithPhones(t)
	key := bytes.Repeat([]byte{4}, pushseal.SecretSize)
	if _, err := store.SetPushKey(iphone, key); err != nil {
		t.Fatal(err)
	}
	send := func(level int) pushgate.PushRequest {
		t.Helper()
		if err := store.SetClient(iphone, device.Client{Platform: "ios", Features: map[string]int{device.FeaturePushSeal: level}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SetPushKey(iphone, key); err != nil {
			t.Fatal(err)
		}
		sender := &pushtest.Sender{}
		n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
		n.Plugin(context.Background(), PluginNotice{Plugin: "vercel", Root: "/Users/me/web", Title: "Deploy failed", Body: "web: build error", Page: "deploys.ui"})
		if len(sender.Calls) == 0 {
			return pushgate.PushRequest{}
		}
		return sender.Calls[0]
	}

	if plain := send(pushseal.FeatureLiveActivities); plain.Token != "" {
		t.Fatalf("level 1 got %s", plain.Payload)
	}

	req := send(pushseal.FeatureAlerts)
	for _, leak := range []string{"Deploy", "web", "vercel", "/Users/me", "deploys.ui"} {
		if strings.Contains(string(req.Payload), leak) || strings.Contains(req.CollapseID, leak) {
			t.Fatalf("%q in the clear: %s / %q", leak, req.Payload, req.CollapseID)
		}
	}
	var body struct {
		APS struct {
			Alert            map[string]string `json:"alert"`
			MutableContent   int               `json:"mutable-content"`
			ContentAvailable int               `json:"content-available"`
		} `json:"aps"`
		HostID string            `json:"host_id"`
		Sealed pushseal.Envelope `json:"sealed"`
	}
	if err := json.Unmarshal(req.Payload, &body); err != nil {
		t.Fatal(err)
	}
	host := string(store.Identity().ID)
	// Only a turn's end wakes the app to fetch.
	if body.APS.MutableContent != 1 || body.APS.ContentAvailable != 0 || body.APS.Alert["title"] != "RepoGo" || body.HostID != host {
		t.Fatalf("visible part %s", req.Payload)
	}
	plainBody, err := pushseal.Open(key, host, pushseal.Alert, "", body.Sealed)
	if err != nil {
		t.Fatal(err)
	}
	var alert sealedAlert
	_ = json.Unmarshal(plainBody, &alert)
	if alert.Title != "Deploy failed" || alert.Thread != "plugin:vercel" || alert.Fields["page"] != "deploys.ui" || alert.Fields["host_id"] != host {
		t.Fatalf("opened %+v", alert)
	}
	if req.CollapseID == "" || req.CollapseID != pushseal.CollapseID(key, "vercel:Deploy failed") {
		t.Fatalf("collapse %q", req.CollapseID)
	}
}

// A turn's end reaches only phones that open sealed alerts, reading as the
// app's own alert: the user's message, who ended where, the recap.
func TestTurnEndedIsSealedOnly(t *testing.T) {
	store, iphone := storeWithPhones(t)
	if err := store.SetClient(iphone, device.Client{Platform: "ios"}); err != nil {
		t.Fatal(err)
	}
	sender := &pushtest.Sender{}
	n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	end := TurnEnd{ChatID: "claude:s1", Agent: "claude", Project: "web", Kind: "finished",
		Prompt: "Fix the login\nbug please", Summary: "Fixed the token refresh; tests pass."}

	n.TurnEnded(context.Background(), end)
	if len(sender.Calls) != 0 {
		t.Fatalf("a phone without a key got %s", sender.Calls[0].Payload)
	}

	key := bytes.Repeat([]byte{6}, pushseal.SecretSize)
	if err := store.SetClient(iphone, device.Client{Platform: "ios", Features: map[string]int{device.FeaturePushSeal: pushseal.FeatureAlerts}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetPushKey(iphone, key); err != nil {
		t.Fatal(err)
	}
	n.TurnEnded(context.Background(), end)
	if len(sender.Calls) != 1 || strings.Contains(string(sender.Calls[0].Payload), "login") {
		t.Fatalf("calls %d: %s", len(sender.Calls), sender.Calls[0].Payload)
	}
	var body struct {
		APS struct {
			MutableContent   int `json:"mutable-content"`
			ContentAvailable int `json:"content-available"`
		} `json:"aps"`
		Sealed pushseal.Envelope `json:"sealed"`
	}
	_ = json.Unmarshal(sender.Calls[0].Payload, &body)
	// It also wakes the app, which fetches the chat's new rows.
	if body.APS.MutableContent != 1 || body.APS.ContentAvailable != 1 {
		t.Fatalf("aps %s", sender.Calls[0].Payload)
	}
	plain, err := pushseal.Open(key, string(store.Identity().ID), pushseal.Alert, "", body.Sealed)
	if err != nil {
		t.Fatal(err)
	}
	var alert sealedAlert
	_ = json.Unmarshal(plain, &alert)
	if alert.Title != "Fix the login bug please" || alert.Subtitle != "Claude finished in web" ||
		alert.Body != "Fixed the token refresh; tests pass." || alert.Fields["kind"] != "turn_end" || alert.Fields["chat_id"] != "claude:s1" {
		t.Fatalf("opened %+v", alert)
	}
}
