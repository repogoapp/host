package push

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/power"
	"github.com/repogoapp/host/internal/push/pushtest"
	"github.com/repogoapp/host/internal/pushseal"
)

func storeWithPhones(t *testing.T) (*device.Store, device.ID) {
	t.Helper()
	store, err := device.Open(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	withToken, _ := device.Generate()
	silent, _ := device.Generate()
	for _, id := range []*device.Identity{withToken, silent} {
		if err := store.Add(device.Peer{ID: id.ID, Public: id.Public}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RegisterPush(withToken.ID, "", withToken.GrantPush(store.Identity().ID, device.PushTarget{Service: device.PushAPNs, Token: "abcd", Environment: "sandbox"})); err != nil {
		t.Fatal(err)
	}
	keyed(t, store, withToken.ID)
	return store, withToken.ID
}

// testKey is every test phone's push key: nothing goes out unsealed.
var testKey = bytes.Repeat([]byte{7}, pushseal.SecretSize)

// keyed has a paired phone open sealed alerts, as the app does on connect.
func keyed(t *testing.T, store *device.Store, id device.ID) {
	t.Helper()
	if err := store.SetClient(id, device.Client{Platform: "ios", Features: map[string]int{device.FeaturePushSeal: pushseal.FeatureAlerts}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetPushKey(id, testKey); err != nil {
		t.Fatal(err)
	}
}

// opened is a sealed alert's content: an iOS body's box, or an FCM data
// message's, as the phone opens it.
func opened(t *testing.T, store *device.Store, payload []byte) sealedAlert {
	t.Helper()
	host := string(store.Identity().ID)
	var ios struct {
		Sealed pushseal.Envelope `json:"sealed"`
	}
	if json.Unmarshal(payload, &ios) == nil && ios.Sealed.Box != "" {
		plain, err := pushseal.Open(testKey, host, pushseal.Alert, "", ios.Sealed)
		if err != nil {
			t.Fatalf("alert does not open: %v", err)
		}
		var a sealedAlert
		_ = json.Unmarshal(plain, &a)
		return a
	}
	t.Fatalf("not a sealed alert: %s", payload)
	return sealedAlert{}
}

// openedData is a sealed FCM data message's map.
func openedData(t *testing.T, store *device.Store, payload []byte) map[string]string {
	t.Helper()
	var outer map[string]string
	if err := json.Unmarshal(payload, &outer); err != nil || len(outer) != 4 {
		t.Fatalf("not a sealed data message: %s", payload)
	}
	plain, err := pushseal.Open(testKey, string(store.Identity().ID), pushseal.FCM, "", pushseal.Envelope{V: 1, KID: outer["kid"], Box: outer["box"]})
	if err != nil {
		t.Fatalf("data message does not open: %v", err)
	}
	var data map[string]string
	_ = json.Unmarshal(plain, &data)
	return data
}

func TestOnlyPeersWithTokensAreNotified(t *testing.T) {
	store, _ := storeWithPhones(t)
	sender := &pushtest.Sender{}
	n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))

	n.NeedsYouAlert(context.Background(), NeedsYou{ChatID: "claude:s1", Agent: "claude", Title: "Task finished", Ask: "Bash: npm test"})

	if len(sender.Calls) != 1 {
		t.Fatalf("sent %d pushes, want 1", len(sender.Calls))
	}
	call := sender.Calls[0]
	if call.Token != "abcd" || call.Environment != "sandbox" || call.CollapseID != pushseal.CollapseID(testKey, "claude:s1") {
		t.Fatalf("push = %+v", call)
	}
	a := opened(t, store, call.Payload)
	if a.Title != "Task finished" || a.Thread != "claude:s1" || a.Fields["chat_id"] != "claude:s1" || a.Fields["host_id"] != string(store.Identity().ID) {
		t.Errorf("alert %+v", a)
	}
	// The relay and Apple read the rest; the chat isn't in it.
	if strings.Contains(string(call.Payload), "claude:s1") || strings.Contains(string(call.Payload), "Task finished") {
		t.Errorf("payload leaks the chat: %s", call.Payload)
	}
}

func TestDeadTokenIsForgotten(t *testing.T) {
	store, id := storeWithPhones(t)
	sender := &pushtest.Sender{Reply: &jsonrpc.Error{Code: jsonrpc.CodeNotFound, Message: "apns: unregistered"}}
	n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))

	n.NeedsYouAlert(context.Background(), NeedsYou{ChatID: "claude:s1", Title: "x", Ask: "y"})

	if peer, _ := store.Peer(id); peer.Push != nil {
		t.Fatal("dead token still stored")
	}
}

// What an approval asks reads as the in-app alert: a question's text, or the
// call and what it acts on.
func TestAskOf(t *testing.T) {
	cases := []struct {
		a        *agent.Approval
		ask      string
		question bool
	}{
		{&agent.Approval{Title: "Bash", Kind: "permission", Input: json.RawMessage(`{"command":"npm test"}`)}, "Bash: npm test", false},
		{&agent.Approval{Title: "Edit", Kind: "permission", Input: json.RawMessage(`{"file_path":"/w/a.go","old":"x"}`)}, "Edit: /w/a.go", false},
		{&agent.Approval{Title: "Permission required", Kind: "permission"}, "Permission required", false},
		{&agent.Approval{Title: "Question", Kind: agent.ApprovalQuestion, Questions: []agent.Question{{Text: "Which database?"}, {Text: "Region?"}}}, "Which database? (+1 more)", true},
		{nil, "", false},
	}
	for _, c := range cases {
		if ask, q := AskOf(c.a); ask != c.ask || q != c.question {
			t.Errorf("AskOf(%+v) = %q, %v; want %q, %v", c.a, ask, q, c.ask, c.question)
		}
	}
}

// A chat's alert goes to the phone that sent its turn alone, and to every
// phone when none sent it, or the sender can't take it.
func TestChatAlertsGoToTheSender(t *testing.T) {
	store, first := storeWithPhones(t)
	second, _ := device.Generate()
	if err := store.Add(device.Peer{ID: second.ID, Public: second.Public}); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterPush(second.ID, "", second.GrantPush(store.Identity().ID, device.PushTarget{Service: device.PushAPNs, Token: "ef01", Environment: "sandbox"})); err != nil {
		t.Fatal(err)
	}
	keyed(t, store, second.ID)
	// A paired phone with no push token, and one that was removed.
	var silent device.ID
	for _, p := range store.Peers() {
		if p.Push == nil {
			silent = p.ID
		}
	}
	gone, _ := device.Generate()

	cases := []struct {
		name   string
		sender device.ID
		tokens []string
	}{
		{"sender", second.ID, []string{"ef01"}},
		{"other sender", first, []string{"abcd"}},
		{"no sender", "", []string{"abcd", "ef01"}},
		{"sender without a token", silent, []string{"abcd", "ef01"}},
		{"removed sender", gone.ID, []string{"abcd", "ef01"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sender := &pushtest.Sender{}
			n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
			n.TurnEnded(context.Background(), TurnEnd{ChatID: "claude:s1", Sender: c.sender, Kind: "finished"})
			n.NeedsYouAlert(context.Background(), NeedsYou{ChatID: "claude:s1", Sender: c.sender, Ask: "Bash: ls"})
			var got []string
			for _, call := range sender.Calls {
				got = append(got, call.Token)
			}
			slices.Sort(got)
			want := append(append([]string{}, c.tokens...), c.tokens...)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("pushed to %v, want %v", got, want)
			}
		})
	}
}

// A chat waiting on the user alerts with its title, who asks where, and what.
func TestNeedsYouReadsAsTheInAppAlert(t *testing.T) {
	store, _ := storeWithPhones(t)
	sender := &pushtest.Sender{}
	n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.NeedsYouAlert(context.Background(), NeedsYou{ChatID: "codex:c1", Agent: "codex", Project: "web",
		Title: "Deploy web", Prompt: "ship it", Question: true, Ask: "Which region? (+1 more)"})
	a := opened(t, store, sender.Calls[0].Payload)
	if a.Title != "Deploy web" || a.Subtitle != "Codex has a question in web" || a.Body != "Which region? (+1 more)" ||
		a.Fields["chat_id"] != "codex:c1" || a.Fields["kind"] != "needs_you" {
		t.Fatalf("alert %+v", a)
	}
	if sent := time.UnixMilli(a.At); time.Since(sent) > time.Minute || time.Since(sent) < 0 {
		t.Fatalf("sent at %v", sent)
	}
}

func TestLowBatteryAlertsOnceUntilRecovered(t *testing.T) {
	store, _ := storeWithPhones(t)
	sender := &pushtest.Sender{}
	n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	on := func(percent int) power.Battery { return power.Battery{Present: true, Percent: percent} }

	for _, b := range []power.Battery{on(20), on(10), on(9), on(8), on(12)} {
		n.Battery(ctx, b)
	}
	if len(sender.Calls) != 1 {
		t.Fatalf("sent %d pushes draining to 8%%, want 1", len(sender.Calls))
	}
	call := sender.Calls[0]
	if a := opened(t, store, call.Payload); call.CollapseID != pushseal.CollapseID(testKey, "battery") || a.Title != "Low battery" ||
		!strings.Contains(a.Body, "9%") {
		t.Fatalf("push = %+v, alert %+v", call, a)
	}

	// Back above the re-arm line, then down again: a second alert.
	n.Battery(ctx, on(16))
	n.Battery(ctx, on(5))
	// Plugged in re-arms too; charging at a low level never alerts.
	n.Battery(ctx, power.Battery{Present: true, Percent: 5, Charging: true, PluggedIn: true})
	n.Battery(ctx, power.Battery{Present: true, Percent: 3, PluggedIn: true})
	n.Battery(ctx, on(3))
	// A desktop Mac has no battery to run low.
	n.Battery(ctx, power.Battery{PluggedIn: true})
	if len(sender.Calls) != 3 {
		t.Fatalf("sent %d pushes, want 3", len(sender.Calls))
	}
}

func TestAndroidGetsTheAlertAsADataMessage(t *testing.T) {
	store, _ := storeWithPhones(t)
	android, _ := device.Generate()
	if err := store.Add(device.Peer{ID: android.ID, Public: android.Public}); err != nil {
		t.Fatal(err)
	}
	fcm := android.GrantPush(store.Identity().ID, device.PushTarget{Service: device.PushFCM, Token: "fJ3x:APA91b"})
	if err := store.RegisterPush(android.ID, "", fcm); err != nil {
		t.Fatal(err)
	}
	keyed(t, store, android.ID)
	sender := &pushtest.Sender{}
	n := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))

	n.NeedsYouAlert(context.Background(), NeedsYou{ChatID: "claude:s1", Agent: "claude", Title: "Approval needed", Ask: "Bash: npm test"})
	n.BrowserRequest(context.Background(), android.ID, "r1")

	var toAndroid []map[string]string
	for _, call := range sender.Calls {
		if call.Service != device.PushFCM {
			if !strings.Contains(string(call.Payload), `"aps"`) {
				t.Errorf("iOS got %s", call.Payload)
			}
			continue
		}
		data := openedData(t, store, call.Payload)
		if call.Priority != 10 || call.CollapseID != "" {
			t.Errorf("android push %+v", call)
		}
		toAndroid = append(toAndroid, data)
	}
	if len(toAndroid) != 2 {
		t.Fatalf("android got %d", len(toAndroid))
	}
	if a := toAndroid[0]; a["kind"] != "alert" || a["title"] != "Approval needed" || a["chat_id"] != "claude:s1" ||
		a["collapse"] != "claude:s1" || a["host_id"] != string(store.Identity().ID) {
		t.Errorf("alert %v", a)
	}
	if b := toAndroid[1]; b["kind"] != "alert" || b["alert_kind"] != "browser_request" || b["request_id"] != "r1" {
		t.Errorf("browser request %v", b)
	}
}
