package liveactivity

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/push/pushtest"
	"github.com/repogo/host/internal/relay"
)

// aps decodes a sent payload's `aps` dictionary.
func aps(t *testing.T, req relay.PushRequest) map[string]any {
	t.Helper()
	var body struct {
		APS map[string]any `json:"aps"`
	}
	if err := json.Unmarshal(req.Payload, &body); err != nil {
		t.Fatal(err)
	}
	return body.APS
}

func state(t *testing.T, req relay.PushRequest) map[string]any {
	t.Helper()
	s, _ := aps(t, req)["content-state"].(map[string]any)
	return s
}

func setup(t *testing.T) (*Driver, *pushtest.Sender, *device.Store, device.ID) {
	t.Helper()
	store, err := device.Open(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	phone, _ := device.Generate()
	if err := store.Add(device.Peer{ID: phone.ID, Public: phone.Public}); err != nil {
		t.Fatal(err)
	}
	phoneKeysMu.Lock()
	phoneKeys[phone.ID] = phone
	phoneKeysMu.Unlock()
	if err := store.RegisterPush(phone.ID, device.PushToStart, "", granted(store, phone.ID, device.PushTarget{Token: "57a4", Environment: "sandbox"})); err != nil {
		t.Fatal(err)
	}
	sender := &pushtest.Sender{}
	d := New(store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)), func(kind agent.Kind) string { return "Claude" }, filepath.Base)
	d.spawn = func(f func()) { f() } // deliver before Observe returns
	return d, sender, store, phone.ID
}

var at = time.UnixMilli(1_700_000_000_000)

func notice(event string, mutate ...func(*notify.Notice)) notify.Notice {
	n := notify.Notice{Agent: agent.KindClaude, SessionID: "s1", Cwd: "/Users/me/app", Event: event, At: at}
	for _, m := range mutate {
		m(&n)
	}
	return n
}

func tool(name, id, input string) func(*notify.Notice) {
	return func(n *notify.Notice) {
		n.ToolName, n.ToolUseID, n.ToolInput = name, id, json.RawMessage(input)
	}
}

func TestPromptStartsOneActivityPerPhone(t *testing.T) {
	d, sender, store, _ := setup(t)
	ctx := context.Background()

	d.Observe(ctx, notice("UserPromptSubmit", func(n *notify.Notice) {
		n.Status, n.Prompt = agent.ChatWorking, "  Fix the\nlogin bug "
	}))
	if len(sender.Calls) != 1 {
		t.Fatalf("sent %d pushes, want 1", len(sender.Calls))
	}
	start := sender.Calls[0]
	if start.Token != "57a4" || start.PushType != relay.PushTypeLiveActivity || start.Priority != 10 {
		t.Fatalf("start push = %+v", start)
	}
	body := aps(t, start)
	attrs, _ := body["attributes"].(map[string]any)
	key := "claude:s1@" + string(store.Identity().ID)
	alert, _ := body["alert"].(map[string]any)
	if body["event"] != "start" || body["attributes-type"] != "LiveActivityAttributes" || attrs["chatId"] != key ||
		body["input-push-token"] != float64(1) || alert["title"] != "Fix the login bug" {
		t.Fatalf("start aps = %v", body)
	}
	if s := state(t, start); s["title"] != "Fix the login bug" || s["status"] != "running" || s["workspaceLabel"] != "app" {
		t.Fatalf("start state = %v", s)
	}

	// The phone has not reported the activity's token yet; the turn must not
	// start a second activity on it meanwhile.
	d.Observe(ctx, notice("PermissionRequest", tool("Bash", "t1", `{"command":"rm -rf build"}`),
		func(n *notify.Notice) { n.Status = agent.ChatAwaitingApproval }))
	if len(sender.Calls) != 1 {
		t.Fatalf("sent %d pushes, want still 1", len(sender.Calls))
	}
}

func TestToolCallsUpdateTheActivity(t *testing.T) {
	d, sender, store, phone := setup(t)
	ctx := context.Background()
	key := "claude:s1@" + string(store.Identity().ID)
	if err := store.RegisterPush(phone, "", key, granted(store, phone, device.PushTarget{Token: "ac71", Environment: "sandbox"})); err != nil {
		t.Fatal(err)
	}

	d.Observe(ctx, notice("UserPromptSubmit", func(n *notify.Notice) { n.Prompt = "Refactor" }))
	// Past the update interval, so the tool call goes out rather than waiting.
	d.now = func() time.Time { return time.Now().Add(updateInterval) }
	d.Observe(ctx, notice("PostToolUse", tool("Edit", "t1",
		`{"file_path":"/Users/me/app/main.go","old_string":"a\nb\nc","new_string":"a\nB\nB2\nc"}`)))

	if len(sender.Calls) != 2 {
		t.Fatalf("sent %d pushes, want 2", len(sender.Calls))
	}
	update := sender.Calls[1]
	if update.Token != "ac71" || aps(t, update)["event"] != "update" {
		t.Fatalf("update = %+v", update)
	}
	s := state(t, update)
	current, _ := s["currentEvent"].(map[string]any)
	lbl, _ := current["label"].(map[string]any)
	if lbl["title"] != "Replaced main.go" || current["additions"] != 2.0 || current["deletions"] != 1.0 {
		t.Fatalf("current event = %v", current)
	}
	if s["fileCount"] != 1.0 || s["additions"] != 2.0 || s["deletions"] != 1.0 {
		t.Fatalf("totals = %v", s)
	}
}

func TestToolUpdatesWaitTheirTurn(t *testing.T) {
	d, sender, store, phone := setup(t)
	ctx := context.Background()
	key := "claude:s1@" + string(store.Identity().ID)
	_ = store.RegisterPush(phone, "", key, granted(store, phone, device.PushTarget{Token: "ac71", Environment: "sandbox"}))

	d.Observe(ctx, notice("UserPromptSubmit"))
	d.Observe(ctx, notice("PreToolUse", tool("Read", "t1", `{"file_path":"/a/b.go"}`)))
	if len(sender.Calls) != 1 {
		t.Fatalf("a tool call right after the start was pushed at once (%d pushes)", len(sender.Calls))
	}
	d.mu.Lock()
	scheduled := d.chats["claude:s1"].timer != nil
	d.mu.Unlock()
	if !scheduled {
		t.Fatal("the held update was not scheduled")
	}
}

func TestApprovalCarriesTheButtonsAnswers(t *testing.T) {
	d, sender, store, phone := setup(t)
	ctx := context.Background()
	key := "claude:s1@" + string(store.Identity().ID)
	_ = store.RegisterPush(phone, "", key, granted(store, phone, device.PushTarget{Token: "ac71", Environment: "sandbox"}))

	d.Observe(ctx, notice("UserPromptSubmit"))
	d.Observe(ctx, notice("PermissionRequest", tool("Bash", "t1", `{"command":"make","description":"Build the app"}`),
		func(n *notify.Notice) { n.Status = agent.ChatAwaitingApproval }))
	d.Approval(ctx, "claude:s1", "turn-9", &agent.Approval{CallID: "call-3", Title: "make", Options: []agent.ApprovalOption{
		{OptionID: "o1", Kind: "allow_once"}, {OptionID: "o2", Kind: "allow_always"}, {OptionID: "o3", Kind: "reject_once"},
	}})

	s := state(t, sender.Calls[len(sender.Calls)-1])
	options, _ := s["approvalOptions"].(map[string]any)
	if s["awaitingApproval"] != true || s["approvalTitle"] != "Build the app" || s["approvalId"] != "call-3" ||
		s["turnId"] != "turn-9" || options["allow_once"] != "o1" || options["reject_once"] != "o3" {
		t.Fatalf("approval state = %v", s)
	}

	d.Approval(ctx, "claude:s1", "turn-9", nil)
	if s := state(t, sender.Calls[len(sender.Calls)-1]); s["awaitingApproval"] != nil {
		t.Fatalf("withdrawn approval still shown: %v", s)
	}
}

func TestStopSettlesWithTheTurnsLength(t *testing.T) {
	d, sender, store, phone := setup(t)
	ctx := context.Background()
	key := "claude:s1@" + string(store.Identity().ID)
	_ = store.RegisterPush(phone, "", key, granted(store, phone, device.PushTarget{Token: "ac71", Environment: "sandbox"}))

	d.Observe(ctx, notice("UserPromptSubmit"))
	d.Observe(ctx, notice("Stop", func(n *notify.Notice) {
		n.Status, n.At = agent.ChatCompleted, at.Add(72*time.Second)
	}))
	s := state(t, sender.Calls[len(sender.Calls)-1])
	if s["status"] != "completed" || s["durationMs"] != 72000.0 || s["currentEvent"] != nil {
		t.Fatalf("settled state = %v", s)
	}
}

func TestDeadActivityTokenIsForgotten(t *testing.T) {
	d, sender, store, phone := setup(t)
	key := "claude:s1@" + string(store.Identity().ID)
	_ = store.RegisterPush(phone, "", key, granted(store, phone, device.PushTarget{Token: "ac71", Environment: "sandbox"}))
	sender.Reply = &jsonrpc.Error{Code: jsonrpc.CodeNotFound, Message: "apns: unregistered"}

	d.Observe(context.Background(), notice("UserPromptSubmit"))
	for _, p := range store.Peers() {
		if _, ok := p.Activities[key]; ok {
			t.Fatal("dead activity token kept")
		}
	}
}

func TestLabels(t *testing.T) {
	for _, tc := range []struct {
		tool, input, title string
	}{
		{"Read", `{"file_path":"/x/y/config.json"}`, "Reading config.json"},
		{"Bash", `{"command":"go test ./...","description":"Run the tests"}`, "Run the tests"},
		{"Grep", `{"pattern":"TODO"}`, "Searching TODO"},
		{"mcp__mcp_stripe_com__create_customer", `{}`, "Create Customer"},
		{"SomethingNew", `{}`, "Running Something New"},
	} {
		if got := labelFor(tc.tool, json.RawMessage(tc.input)).Title; got != tc.title {
			t.Errorf("%s: title %q, want %q", tc.tool, got, tc.title)
		}
	}
	if got := completedTitle("Reading config.json"); got != "Read config.json" {
		t.Errorf("completed title %q", got)
	}
	if a, r := lineChanges("Write", json.RawMessage(`{"content":"a\nb\n"}`)); a != 2 || r != 0 {
		t.Errorf("write changes +%d -%d", a, r)
	}
}

// phoneKeys holds each test phone's identity, so a token can be registered
// with the grant the real phone would sign.
var (
	phoneKeysMu sync.Mutex
	phoneKeys   = map[device.ID]*device.Identity{}
)

func granted(store *device.Store, phone device.ID, target device.PushTarget) device.PushTarget {
	phoneKeysMu.Lock()
	defer phoneKeysMu.Unlock()
	return phoneKeys[phone].GrantPush(store.Identity().ID, target)
}
