package liveactivity

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/pushgate"
	"github.com/repogoapp/host/internal/pushseal"
	"github.com/repogoapp/host/internal/store"
)

func TestSealedPhoneGetsOnlyTheBox(t *testing.T) {
	f := setup(t)
	key := testKey
	f.set(agent.ChatWorking, "claude:s1")

	event, _, _, aps := f.last()
	if event != "start" {
		t.Fatalf("event %s", event)
	}
	var state struct {
		Sealed pushseal.Envelope `json:"sealed"`
	}
	if err := json.Unmarshal(aps["content-state"], &state); err != nil || state.Sealed.Box == "" {
		t.Fatalf("content-state %s", aps["content-state"])
	}
	if strings.Contains(string(aps["content-state"]), "working") {
		t.Fatalf("counts in the clear: %s", aps["content-state"])
	}
	host := string(f.devices.Identity().ID)
	plain, err := pushseal.Open(key, host, pushseal.AgentsState, "", state.Sealed)
	if err != nil {
		t.Fatal(err)
	}
	var got sealedCounts
	_ = json.Unmarshal(plain, &got)
	if got.Counts != (Counts{Working: 1}) || got.At != f.now.UnixMilli() {
		t.Fatalf("opened %s", plain)
	}
}

// A phone that sent no key gets no card at all; the one beside it with a key
// gets its sealed card in the same round.
func TestPhoneWithoutKeyGetsNothing(t *testing.T) {
	f := setup(t)
	f.android("fcm:1")
	if err := f.devices.SetClient(f.phone.ID, device.Client{Platform: "ios"}); err != nil {
		t.Fatal(err)
	}
	f.set(agent.ChatWorking, "claude:s1")
	for _, req := range f.sender.Calls {
		if req.Service != pushgate.ServiceFCM {
			t.Fatalf("the iPhone with no key got %s", req.Payload)
		}
	}
	if pushes := f.androidPushes(); len(pushes) != 1 || pushes[0]["kind"] != "agents" || pushes[0]["event"] != "start" {
		t.Fatalf("android got %v", pushes)
	}
}

// An app downgraded to one without the feature loses its key, and its card.
func TestHelloWithoutFeatureDropsTheKey(t *testing.T) {
	f := setup(t)
	if err := f.devices.SetClient(f.phone.ID, device.Client{Platform: "ios"}); err != nil {
		t.Fatal(err)
	}
	f.set(agent.ChatWorking, "claude:s1")
	if len(f.sender.Calls) != 0 {
		t.Fatalf("pushed %s", f.sender.Calls[0].Payload)
	}
	if p, _ := f.devices.Peer(f.phone.ID); p.PushKey != nil {
		t.Fatal("key kept")
	}
}

// A sealed card lists its chats, needing the user first; a title that
// changes with the counts unchanged still pushes.
func TestSealedCardListsChats(t *testing.T) {
	f := setup(t)
	key := testKey
	f.rows["claude:a"] = store.Chat{ID: "claude:a", Agent: "claude", Title: "Fix the login bug", Status: agent.ChatWorking, StatusAt: 1}
	f.rows["codex:b"] = store.Chat{ID: "codex:b", Agent: "codex", Title: "Deploy web", Status: agent.ChatAwaitingApproval, StatusAt: 2}
	f.d.observe("claude:a")
	f.d.observe("codex:b")
	f.d.update(context.Background())

	open := func() sealedCounts {
		t.Helper()
		_, _, _, aps := f.last()
		var state struct {
			Sealed pushseal.Envelope `json:"sealed"`
		}
		_ = json.Unmarshal(aps["content-state"], &state)
		plain, err := pushseal.Open(key, string(f.devices.Identity().ID), pushseal.AgentsState, "", state.Sealed)
		if err != nil {
			t.Fatal(err)
		}
		var got sealedCounts
		_ = json.Unmarshal(plain, &got)
		return got
	}
	got := open()
	want := []Row{{"Deploy web", RowApproval}, {"Fix the login bug", RowWorking}}
	if !slices.Equal(got.Chats, want) || got.Counts != (Counts{Working: 1, NeedsYou: 1}) {
		t.Fatalf("card %+v", got)
	}

	f.register(device.PushActivity, "ac71")
	sent := len(f.sender.Calls)
	f.now = f.now.Add(updateInterval)
	f.rows["claude:a"] = store.Chat{ID: "claude:a", Agent: "claude", Title: "Fix the login bug on iOS", Status: agent.ChatWorking, StatusAt: 3}
	f.d.observe("claude:a")
	f.d.update(context.Background())
	if len(f.sender.Calls) == sent || open().Chats[1].Title != "Fix the login bug on iOS" {
		t.Fatal("a new title didn't push")
	}
}
