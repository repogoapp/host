package stepactivity

import (
	"encoding/json"
	"testing"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/pushgate"
)

// android pairs an Android phone opted into plugins' activities with its one FCM token.
func (f *fixture) android(token string) {
	f.t.Helper()
	phone, _ := device.Generate()
	if err := f.devices.Add(device.Peer{ID: phone.ID, Public: phone.Public}); err != nil {
		f.t.Fatal(err)
	}
	target := phone.GrantPush(f.devices.Identity().ID, device.PushTarget{Service: device.PushFCM, Token: token})
	if err := f.devices.RegisterPush(phone.ID, device.PushStepsStart, target); err != nil {
		f.t.Fatal(err)
	}
}

// androidPushes are the data messages sent to FCM, in order, each with its priority.
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

func TestAndroidGetsTheActivityAsDataMessages(t *testing.T) {
	f := setup(t)
	f.android("fcm:1")
	f.set("vercel", deploy)

	pushes := f.androidPushes()
	if len(pushes) != 1 {
		t.Fatalf("%d android pushes", len(pushes))
	}
	start := pushes[0]
	var state State
	_ = json.Unmarshal([]byte(start["state"]), &state)
	if start["kind"] != "steps" || start["event"] != "start" || start["priority"] != "high" ||
		start["key"] != "vercel/deploy:web" || start["project"] != "/Users/me/web" ||
		state.Title != "Deploying web" || state.Progress.Current != 0 {
		t.Fatalf("start %v, state %+v", start, state)
	}

	f.now = f.now.Add(updateInterval)
	f.set("vercel", deployAt(2, "Deploying", ""))
	update := f.androidPushes()[1]
	_ = json.Unmarshal([]byte(update["state"]), &state)
	if update["event"] != "update" || update["priority"] != "normal" || update["generation"] != start["generation"] ||
		update["revision"] <= start["revision"] || state.Progress.Current != 2 {
		t.Fatalf("update %v, state %+v", update, state)
	}

	f.now = f.now.Add(minGap)
	f.set("vercel", deployAt(4, "Live", Done))
	end := f.androidPushes()[2]
	_ = json.Unmarshal([]byte(end["state"]), &state)
	if end["event"] != "end" || end["priority"] != "high" || state.Status != Done || state.EndedAt == 0 {
		t.Fatalf("end %v, state %+v", end, state)
	}
	if len(f.d.live) != 0 {
		t.Error("activity still live")
	}
}
