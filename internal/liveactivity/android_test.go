package liveactivity

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/pushgate"
)

// android pairs an Android phone opted into the card with its one FCM token.
func (f *fixture) android(token string) {
	f.t.Helper()
	phone, _ := device.Generate()
	if err := f.devices.Add(device.Peer{ID: phone.ID, Public: phone.Public}); err != nil {
		f.t.Fatal(err)
	}
	target := phone.GrantPush(f.devices.Identity().ID, device.PushTarget{Service: device.PushFCM, Token: token})
	if err := f.devices.RegisterPush(phone.ID, device.PushToStart, target); err != nil {
		f.t.Fatal(err)
	}
}

// androidPushes are the data messages sent to FCM, in order.
func (f *fixture) androidPushes() []map[string]string {
	var out []map[string]string
	for _, req := range f.sender.Calls {
		if req.Service != pushgate.ServiceFCM {
			continue
		}
		var data map[string]string
		if err := json.Unmarshal(req.Payload, &data); err != nil {
			f.t.Fatalf("not a data message: %s", req.Payload)
		}
		data["priority"] = map[int]string{5: "normal", 10: "high"}[req.Priority]
		out = append(out, data)
	}
	return out
}

// lastAPNs is the newest push to an iPhone: its event and token.
func (f *fixture) lastAPNs() (event, token string) {
	f.t.Helper()
	for i := len(f.sender.Calls) - 1; i >= 0; i-- {
		req := f.sender.Calls[i]
		if req.Service == pushgate.ServiceFCM {
			continue
		}
		var body struct {
			APS struct {
				Event string `json:"event"`
			} `json:"aps"`
		}
		_ = json.Unmarshal(req.Payload, &body)
		return body.APS.Event, req.Token
	}
	f.t.Fatal("no APNs push")
	return "", ""
}

func TestAndroidGetsTheCountsAsDataMessages(t *testing.T) {
	f := setup(t)
	f.android("fcm:1")
	f.set(agent.ChatWorking, "claude:s1")

	pushes := f.androidPushes()
	if len(pushes) != 1 {
		t.Fatalf("%d android pushes", len(pushes))
	}
	start := pushes[0]
	var counts Counts
	_ = json.Unmarshal([]byte(start["state"]), &counts)
	if start["kind"] != "agents" || start["event"] != "start" || start["priority"] != "high" ||
		start["host_id"] != string(f.devices.Identity().ID) || counts != (Counts{Working: 1}) {
		t.Fatalf("start %v", start)
	}
	// The iPhone still gets its APNs start alongside.
	if event, token := f.lastAPNs(); event != "start" || token != "57a4" {
		t.Fatalf("iOS got %q to %q", event, token)
	}

	// A running change waits its interval and goes at normal priority, in
	// the same generation with a later revision.
	f.now = f.now.Add(updateInterval)
	f.set(agent.ChatWorking, "claude:s2")
	update := f.androidPushes()[1]
	if update["event"] != "update" || update["priority"] != "normal" || update["generation"] != start["generation"] ||
		update["revision"] <= start["revision"] {
		t.Fatalf("update %v after %v", update, start)
	}

	// Needing the user wakes the phone.
	f.set(agent.ChatAwaitingApproval, "claude:s2")
	if needs := f.androidPushes()[2]; needs["priority"] != "high" {
		t.Fatalf("needs you %v", needs)
	}

	f.set(agent.ChatCompleted, "claude:s1", "claude:s2")
	f.now = f.now.Add(idleTimeout)
	f.d.push(context.Background())
	pushes = f.androidPushes()
	if end := pushes[len(pushes)-1]; end["event"] != "end" || end["priority"] != "high" {
		t.Fatalf("end %v", end)
	}

	// The next work is a new card: a start in a new generation.
	f.now = f.now.Add(updateInterval)
	f.set(agent.ChatWorking, "claude:s3")
	pushes = f.androidPushes()
	if next := pushes[len(pushes)-1]; next["event"] != "start" || next["generation"] == start["generation"] {
		t.Fatalf("next %v", next)
	}
}

func TestAndroidDeadTokenIsForgotten(t *testing.T) {
	f := setup(t)
	f.android("fcm:1")
	f.d.send = &deadFor{token: "fcm:1"}
	f.set(agent.ChatWorking, "claude:s1")
	for _, p := range f.devices.Peers() {
		if p.PushToStart != nil && p.PushToStart.Service == device.PushFCM {
			t.Fatal("dead fcm token kept")
		}
	}
}
