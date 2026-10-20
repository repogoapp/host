package liveactivity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/push/pushtest"
	"github.com/repogoapp/host/internal/relay"
	"github.com/repogoapp/host/internal/store"
)

// rows stands in for the store: one status per chat.
type rows map[store.ChatID]store.Chat

func (r rows) Info(id store.ChatID) (store.Chat, error) {
	c, ok := r[id]
	if !ok {
		return store.Chat{}, store.ErrNotFound
	}
	return c, nil
}

func (r rows) Active() ([]store.Chat, error) {
	var out []store.Chat
	for _, c := range r {
		if c.Status.Active() {
			out = append(out, c)
		}
	}
	return out, nil
}

type fixture struct {
	t       *testing.T
	d       *Driver
	sender  *pushtest.Sender
	devices *device.Store
	phone   *device.Identity
	rows    rows
	now     time.Time
}

func setup(t *testing.T) *fixture {
	t.Helper()
	devices, err := device.Open(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	phone, _ := device.Generate()
	if err := devices.Add(device.Peer{ID: phone.ID, Public: phone.Public}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, sender: &pushtest.Sender{}, devices: devices, phone: phone, rows: rows{},
		now: time.Now()}
	f.register(device.PushToStart, "57a4")
	f.d = New(f.rows, devices, f.sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.d.now = func() time.Time { return f.now }
	return f
}

func (f *fixture) register(kind, token string) {
	f.t.Helper()
	target := f.phone.GrantPush(f.devices.Identity().ID, device.PushTarget{Service: device.PushAPNs, Token: token, Environment: "sandbox"})
	if err := f.devices.RegisterPush(f.phone.ID, kind, target); err != nil {
		f.t.Fatal(err)
	}
}

// set moves chats to a status and runs what Run does for one wake.
func (f *fixture) set(status agent.ChatStatus, ids ...string) {
	for _, id := range ids {
		kind, _, _ := strings.Cut(id, ":")
		f.rows[store.ChatID(id)] = store.Chat{ID: store.ChatID(id), Agent: kind, Status: status}
		f.d.observe(store.ChatID(id))
	}
	f.d.update(context.Background())
}

// last is the newest push's event, its token and its content-state.
func (f *fixture) last() (event, token string, counts Counts, aps map[string]json.RawMessage) {
	f.t.Helper()
	if len(f.sender.Calls) == 0 {
		f.t.Fatal("nothing pushed")
	}
	req := f.sender.Calls[len(f.sender.Calls)-1]
	if req.PushType != relay.PushTypeLiveActivity {
		f.t.Fatalf("push type %q", req.PushType)
	}
	var body struct {
		APS map[string]json.RawMessage `json:"aps"`
	}
	if err := json.Unmarshal(req.Payload, &body); err != nil {
		f.t.Fatal(err)
	}
	_ = json.Unmarshal(body.APS["event"], &event)
	_ = json.Unmarshal(body.APS["content-state"], &counts)
	return event, req.Token, counts, body.APS
}

func TestFirstWorkingChatStartsTheActivity(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatWorking, "claude:s1")
	event, token, counts, aps := f.last()
	if event != "start" || token != "57a4" {
		t.Fatalf("event %q to %q", event, token)
	}
	if counts != (Counts{Working: 1}) {
		t.Errorf("counts %+v", counts)
	}
	var attributes map[string]string
	_ = json.Unmarshal(aps["attributes"], &attributes)
	if attributes["host_id"] != string(f.devices.Identity().ID) || len(attributes) != 1 {
		t.Errorf("attributes %v", attributes)
	}
	if _, ok := aps["input-push-token"]; !ok {
		t.Error("start asks for no update token")
	}
}

// Apple and the relay read every push, so the card carries numbers and
// nothing else.
func TestPayloadCarriesOnlyCounts(t *testing.T) {
	f := setup(t)
	f.rows["claude:s1"] = store.Chat{ID: "claude:s1", Agent: "claude", Title: "Fix the login secret", CWD: "/Users/me/secret-project"}
	f.set(agent.ChatAwaitingApproval, "claude:s1")
	_, _, _, aps := f.last()
	var state map[string]any
	_ = json.Unmarshal(aps["content-state"], &state)
	keys := slices.Sorted(maps.Keys(state))
	if !slices.Equal(keys, []string{"finished", "needs_you", "working"}) {
		t.Errorf("content-state keys %v", keys)
	}
	raw := string(f.sender.Calls[0].Payload)
	for _, leak := range []string{"secret", "claude:s1", "/Users"} {
		if strings.Contains(raw, leak) {
			t.Errorf("payload holds %q: %s", leak, raw)
		}
	}
}

func TestCountsFollowStatuses(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatWorking, "claude:s1", "claude:s2")
	f.register(device.PushActivity, "a1")

	f.set(agent.ChatAwaitingUser, "claude:s2")
	event, token, counts, _ := f.last()
	if event != "update" || token != "a1" || counts != (Counts{Working: 1, NeedsYou: 1}) {
		t.Fatalf("needing the user went %q to %q with %+v", event, token, counts)
	}

	f.now = f.now.Add(updateInterval)
	f.set(agent.ChatCompleted, "claude:s1")
	if _, _, counts, _ = f.last(); counts != (Counts{NeedsYou: 1, Finished: 1}) {
		t.Errorf("after a finish %+v", counts)
	}

	// A chat that runs again leaves finished.
	f.now = f.now.Add(updateInterval)
	f.set(agent.ChatWorking, "claude:s1")
	if _, _, counts, _ = f.last(); counts != (Counts{Working: 1, NeedsYou: 1}) {
		t.Errorf("after running again %+v", counts)
	}
}

func TestWorkingChangesWaitForTheInterval(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatWorking, "claude:s1")
	f.register(device.PushActivity, "a1")
	f.set(agent.ChatWorking, "claude:s2")
	if want := f.now.Add(updateInterval); len(f.sender.Calls) != 1 || !f.d.wakeAt.Equal(want) {
		t.Fatalf("pushed %d at once, next at %v", len(f.sender.Calls), f.d.wakeAt)
	}
	f.now = f.now.Add(updateInterval)
	f.d.push(context.Background())
	if _, _, counts, _ := f.last(); counts.Working != 2 {
		t.Errorf("held update %+v", counts)
	}
}

func TestIdleActivityEnds(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatWorking, "claude:s1")
	f.register(device.PushActivity, "a1")
	f.set(agent.ChatCompleted, "claude:s1")
	if event, _, counts, _ := f.last(); event != "update" || counts.Finished != 1 {
		t.Fatalf("finish went %q with %+v", event, counts)
	}
	if f.d.wakeAt.IsZero() {
		t.Fatal("no idle end")
	}

	f.now = f.now.Add(idleTimeout)
	f.d.push(context.Background())
	event, _, _, aps := f.last()
	if event != "end" {
		t.Fatalf("idle went %q", event)
	}
	if _, ok := aps["dismissal-date"]; !ok {
		t.Error("end has no dismissal date")
	}

	// The ended card's token is forgotten, so the next working chat starts a
	// fresh card, its finishes forgotten.
	if p, _ := f.devices.Peer(f.phone.ID); p.Activity != nil {
		t.Fatalf("ended token kept: %+v", p.Activity)
	}
	f.set(agent.ChatWorking, "claude:s2")
	if event, _, counts, _ := f.last(); event != "start" || counts.Finished != 0 {
		t.Errorf("next work went %q with %+v", event, counts)
	}
}

func TestFinishAloneStartsNothing(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatCompleted, "claude:s1")
	if len(f.sender.Calls) != 0 {
		t.Errorf("pushed %d", len(f.sender.Calls))
	}
}

// deadFor answers one token as Apple does for a dismissed activity.
type deadFor struct {
	pushtest.Sender
	token string
}

func (s *deadFor) Control(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.Sender.Control(ctx, method, params)
	if params.(relay.PushRequest).Token == s.token {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeNotFound, Message: "Unregistered"}
	}
	return nil, nil
}

// A dismissed card's token is forgotten and the phone gets a new card.
func TestDeadTokenIsForgotten(t *testing.T) {
	f := setup(t)
	f.register(device.PushActivity, "a1")
	sender := &deadFor{token: "a1"}
	f.d.send = sender
	f.set(agent.ChatWorking, "claude:s1")
	p, _ := f.devices.Peer(f.phone.ID)
	if p.Activity != nil {
		t.Errorf("activity token kept: %+v", p.Activity)
	}
	if p.PushToStart == nil {
		t.Error("start token forgotten with it")
	}
	if calls := sender.Calls; len(calls) != 2 || calls[1].Token != "57a4" {
		t.Errorf("no new start after the dead update: %d calls", len(calls))
	}
}

func TestExpiredActivityStartsAgain(t *testing.T) {
	f := setup(t)
	f.register(device.PushActivity, "a1")
	f.now = time.Now().Add(maxActivityAge)
	f.set(agent.ChatWorking, "claude:s1")
	if event, token, _, _ := f.last(); event != "start" || token != "57a4" {
		t.Errorf("past eight hours went %q to %q", event, token)
	}
}

func TestRemovedChatLeavesTheCounts(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatWorking, "claude:s1", "claude:s2")
	delete(f.rows, "claude:s2")
	f.d.observe("claude:s2")
	if c := f.d.counts(); c.Working != 1 {
		t.Errorf("counts %+v", c)
	}
}

// The idle clock starts when the work stops, not at the last push: a turn
// that ran unchanged for longer than the timeout still shows its finish.
func TestIdleCountsFromTheLastFinish(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatWorking, "claude:s1")
	f.register(device.PushActivity, "a1")
	f.now = f.now.Add(2 * idleTimeout)
	f.set(agent.ChatCompleted, "claude:s1")
	if event, _, counts, _ := f.last(); event != "update" || counts.Finished != 1 {
		t.Fatalf("long turn's finish went %q with %+v", event, counts)
	}
	if want := f.now.Add(idleTimeout); !f.d.wakeAt.Equal(want) {
		t.Errorf("idle end at %v, want %v", f.d.wakeAt, want)
	}
}

// Until the phone sends the started activity's token, more changes start
// nothing more: one card, one alert.
func TestOneStartUntilTheTokenArrives(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatWorking, "claude:s1")
	f.now = f.now.Add(updateInterval)
	f.set(agent.ChatAwaitingApproval, "claude:s1")
	if len(f.sender.Calls) != 1 {
		t.Fatalf("pushed %d before the token came", len(f.sender.Calls))
	}
	f.now = f.now.Add(startRetry)
	f.set(agent.ChatWorking, "claude:s1")
	if event, _, _, _ := f.last(); len(f.sender.Calls) != 2 || event != "start" {
		t.Errorf("after the retry window: %d pushes, last %q", len(f.sender.Calls), event)
	}
}

// A card near iOS's eight hours is ended and started again, with no
// status change to prompt it.
func TestLongActivityIsRenewed(t *testing.T) {
	f := setup(t)
	f.register(device.PushActivity, "a1")
	f.set(agent.ChatWorking, "claude:s1")
	if want := f.now.Add(maxActivityAge - renewBefore); f.d.wakeAt.Sub(want).Abs() > time.Second {
		t.Fatalf("renewal at %v, want about %v", f.d.wakeAt, want)
	}
	f.now = f.now.Add(maxActivityAge - renewBefore + time.Second)
	f.d.push(context.Background())
	n := len(f.sender.Calls)
	if n < 3 {
		t.Fatalf("pushed %d", n)
	}
	var events []string
	for _, req := range f.sender.Calls[n-2:] {
		var body struct {
			APS struct{ Event string } `json:"aps"`
		}
		_ = json.Unmarshal(req.Payload, &body)
		events = append(events, body.APS.Event+" "+req.Token)
	}
	if !slices.Equal(events, []string{"end a1", "start 57a4"}) {
		t.Errorf("renewal sent %v", events)
	}
}

// A push the relay could not take is sent again, even with no new change.
func TestFailedPushIsRetried(t *testing.T) {
	f := setup(t)
	f.register(device.PushActivity, "a1")
	f.sender.Reply = errors.New("relay down")
	f.set(agent.ChatWorking, "claude:s1")
	if f.d.last.ok || !f.d.wakeAt.Equal(f.now.Add(updateInterval)) {
		t.Fatalf("delivered %v, retry at %v", f.d.last.ok, f.d.wakeAt)
	}
	f.sender.Reply = nil
	f.now = f.now.Add(updateInterval)
	f.d.push(context.Background())
	if !f.d.last.ok {
		t.Error("retry not delivered")
	}
	if p, _ := f.devices.Peer(f.phone.ID); p.Activity == nil {
		t.Error("a failed push forgot the token")
	}
}

// After a restart the counts come from the store, and a card left showing
// with nothing active ends after the idle timeout.
func TestRestartPicksUp(t *testing.T) {
	f := setup(t)
	f.rows["claude:s1"] = store.Chat{ID: "claude:s1", Agent: "claude", Status: agent.ChatWorking}
	f.register(device.PushActivity, "a1")
	f.d.restore(context.Background())
	if event, _, counts, _ := f.last(); event != "update" || counts.Working != 1 {
		t.Fatalf("restart sent %q with %+v", event, counts)
	}

	g := setup(t)
	g.register(device.PushActivity, "a1")
	g.d.restore(context.Background())
	if !g.d.wakeAt.Equal(g.now.Add(idleTimeout)) {
		t.Fatalf("idle end at %v", g.d.wakeAt)
	}
	g.now = g.now.Add(idleTimeout)
	g.d.push(context.Background())
	if event, _, _, _ := g.last(); event != "end" {
		t.Errorf("left card got %q", event)
	}
}

// Run returns when ctx ends and pushes nothing after.
func TestRunPushesNothingAfterShutdown(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatWorking, "claude:s1")
	f.register(device.PushActivity, "a1")
	f.set(agent.ChatWorking, "claude:s2")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.d.Run(ctx)
	f.d.push(ctx)
	if n := len(f.sender.Calls); n != 1 {
		t.Errorf("pushed %d after shutdown", n-1)
	}
}

// Another write to a finished chat, a rename or a sync, keeps it finished.
func TestFinishedSurvivesLaterWrites(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatWorking, "claude:s1", "claude:s2")
	f.set(agent.ChatCompleted, "claude:s1")
	f.set(agent.ChatCompleted, "claude:s1")
	if c := f.d.counts(); c.Finished != 1 {
		t.Errorf("counts %+v", c)
	}
}

// A read that fails for another reason than a missing chat keeps the counts.
func TestFailedReadKeepsState(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatWorking, "claude:s1")
	f.d.chats = failing{}
	f.d.Changed(store.Change{Changed: []store.ChatID{"claude:s1"}})
	f.d.observePending()
	if c := f.d.counts(); c.Working != 1 {
		t.Errorf("counts %+v", c)
	}
	// The chat stays queued, and the next wake reads it again.
	if !f.d.pending["claude:s1"] || !f.d.wakeAt.Equal(f.now.Add(updateInterval)) {
		t.Errorf("pending %v, next at %v", f.d.pending, f.d.wakeAt)
	}
	f.d.chats = f.rows
	f.rows["claude:s1"] = store.Chat{ID: "claude:s1", Agent: "claude", Status: agent.ChatCompleted}
	f.d.observePending()
	if c := f.d.counts(); c.Working != 0 || c.Finished != 1 {
		t.Errorf("after the retry %+v", c)
	}
}

type failing struct{}

func (failing) Info(store.ChatID) (store.Chat, error) {
	return store.Chat{}, errors.New("database is locked")
}
func (failing) Active() ([]store.Chat, error) { return nil, errors.New("database is locked") }

// A renewal whose end does not go through is retried, and starts nothing
// that would leave the phone with two cards.
func TestFailedRenewalEndStartsNothing(t *testing.T) {
	f := setup(t)
	f.register(device.PushActivity, "a1")
	f.set(agent.ChatWorking, "claude:s1")
	f.sender.Reply = errors.New("relay down")
	f.now = f.now.Add(maxActivityAge)
	f.d.push(context.Background())
	if event, token, _, _ := f.last(); event != "end" || token != "a1" {
		t.Errorf("renewal sent %q to %q", event, token)
	}
	if !f.d.wakeAt.Equal(f.now.Add(updateInterval)) {
		t.Errorf("retry at %v", f.d.wakeAt)
	}
}

// A card whose token arrives after its start gets a renewal time though
// the counts never change.
func TestLateTokenGetsARenewal(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatWorking, "claude:s1")
	if want := f.now.Add(startRetry); !f.d.wakeAt.Equal(want) {
		t.Fatalf("after the start, next at %v, want %v", f.d.wakeAt, want)
	}
	f.register(device.PushActivity, "a1")
	f.now = f.d.wakeAt
	f.d.push(context.Background())
	p, _ := f.devices.Peer(f.phone.ID)
	if event, _, _, _ := f.last(); event != "update" || !f.d.wakeAt.Equal(renewalAt(*p.Activity)) {
		t.Errorf("sent %q, next at %v, want the renewal %v", event, f.d.wakeAt, renewalAt(*p.Activity))
	}
}

// A chat that finishes before the phone sends the started card's token still
// wakes the loop then, so the card shows the finish before it ends.
func TestFinishBeforeTheTokenStillUpdates(t *testing.T) {
	f := setup(t)
	f.set(agent.ChatWorking, "claude:s1")
	started := f.now
	f.set(agent.ChatCompleted, "claude:s1")
	if want := started.Add(startRetry); !f.d.wakeAt.Equal(want) {
		t.Fatalf("next at %v, want the start's retry %v", f.d.wakeAt, want)
	}
	f.register(device.PushActivity, "a1")
	f.now = f.d.wakeAt
	f.d.push(context.Background())
	if event, token, counts, _ := f.last(); event != "update" || token != "a1" || counts.Finished != 1 {
		t.Errorf("sent %q to %q with %+v", event, token, counts)
	}
}

// A reinstall clears the phone's card token; the host starts a new card
// though the counts never changed.
func TestClearedTokenStartsANewCard(t *testing.T) {
	f := setup(t)
	f.register(device.PushActivity, "a1")
	f.set(agent.ChatWorking, "claude:s1")
	if err := f.devices.RegisterPush(f.phone.ID, device.PushActivity, device.PushTarget{}); err != nil {
		t.Fatal(err)
	}
	f.d.TokensChanged()
	f.now = f.now.Add(updateInterval)
	f.d.observePending()
	f.d.update(context.Background())
	if event, token, counts, _ := f.last(); event != "start" || token != "57a4" || counts.Working != 1 {
		t.Errorf("sent %q to %q with %+v", event, token, counts)
	}
}
