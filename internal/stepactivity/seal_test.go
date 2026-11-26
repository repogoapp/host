package stepactivity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/pushseal"
)

// A sealed start carries no plugin text, key or project outside the boxes.
func TestSealedStartHidesThePlugin(t *testing.T) {
	f := setup(t)
	key := testKey
	f.set("vercel", deploy)

	req := f.sender.Calls[len(f.sender.Calls)-1]
	for _, leak := range []string{"Deploying", "Building", "deploy:web", "/Users/me/web", "deploys.ui", "shippingbox"} {
		if strings.Contains(string(req.Payload), leak) {
			t.Fatalf("%q in the clear: %s", leak, req.Payload)
		}
	}
	s := f.last()
	var alert map[string]string
	_ = json.Unmarshal(s.aps["alert"], &alert)
	if s.event != "start" || alert["title"] != "RepoGo" || alert["body"] != "An activity started" {
		t.Fatalf("start %s %v", s.event, alert)
	}
	host := string(f.devices.Identity().ID)
	var attrs sealedAttributes
	_ = json.Unmarshal(s.aps["attributes"], &attrs)
	plain, err := pushseal.Open(key, host, pushseal.StepsAttrs, "", attrs.Sealed)
	if err != nil || attrs.HostID != host || !strings.Contains(string(plain), `"key":"vercel/deploy:web"`) {
		t.Fatalf("attributes %s: %v", s.aps["attributes"], err)
	}
	var state struct {
		Sealed pushseal.Envelope `json:"sealed"`
	}
	_ = json.Unmarshal(s.aps["content-state"], &state)
	// Bound to the activity: it opens only under its own key.
	if _, err := pushseal.Open(key, host, pushseal.StepsState, "vercel/other", state.Sealed); err == nil {
		t.Fatal("opened under another activity")
	}
	plain, err = pushseal.Open(key, host, pushseal.StepsState, "vercel/deploy:web", state.Sealed)
	var got sealedState
	_ = json.Unmarshal(plain, &got)
	if err != nil || got.Title != "Deploying web" || got.At != f.now.UnixMilli() {
		t.Fatalf("state %s: %v", plain, err)
	}
}

// Set refuses an activity whose sealed start is past 4 KB, whichever phones are paired.
func TestSetChecksTheSealedSize(t *testing.T) {
	f := setup(t)
	r := deploy
	r.Project = "/" + strings.Repeat("p", 1000)
	r.Page = strings.Repeat("g", 500)
	r.Title = strings.Repeat("界", 60)
	r.Body = strings.Repeat("界", 120)
	r.Progress = &Progress{Kind: ProgressSteps, Steps: []string{
		strings.Repeat("界", 24), strings.Repeat("界", 24), strings.Repeat("界", 24), strings.Repeat("界", 24),
		strings.Repeat("界", 24), strings.Repeat("界", 24), strings.Repeat("界", 24), strings.Repeat("界", 24)}}
	now := f.now
	act := Attributes{HostID: "h", Key: "vercel/deploy:web", Project: r.Project}
	rd := round{key: act.Key, attrs: act, state: State{Content: r.Content}}
	if sealed, _ := f.d.payload("start", rd, now, sizingKey); len(sealed) <= maxPayload {
		t.Fatalf("the case is %d bytes sealed, within the limit; widen it", len(sealed))
	}
	if err := f.d.Set("vercel", r); err == nil || !strings.Contains(err.Error(), "bytes as a push") {
		t.Fatalf("Set = %v", err)
	}
}
