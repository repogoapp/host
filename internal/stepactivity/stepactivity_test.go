package stepactivity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/push/pushtest"
	"github.com/repogoapp/host/internal/relay"
)

type fixture struct {
	t       *testing.T
	d       *Driver
	sender  *pushtest.Sender
	devices *device.Store
	phone   *device.Identity
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
	f := &fixture{t: t, sender: &pushtest.Sender{}, devices: devices, phone: phone, now: time.Now()}
	f.register(device.PushStepsStart, "", "57a4")
	f.d = New(devices, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.d.send = f.sender
	f.d.now = func() time.Time { return f.now }
	return f
}

func (f *fixture) grant(token string) device.PushTarget {
	return f.phone.GrantPush(f.devices.Identity().ID, device.PushTarget{Service: device.PushAPNs, Token: token, Environment: "sandbox"})
}

// register saves a token as the phone would: a "steps" token by its key.
func (f *fixture) register(kind, key, token string) {
	f.t.Helper()
	var err error
	if kind == device.PushSteps {
		err = f.devices.RegisterStepsPush(f.phone.ID, key, f.grant(token))
	} else {
		err = f.devices.RegisterPush(f.phone.ID, kind, f.grant(token))
	}
	if err != nil {
		f.t.Fatal(err)
	}
}

// set is a plugin's call, then the round Run would push.
func (f *fixture) set(plugin string, r Request) {
	f.t.Helper()
	if err := f.d.Set(plugin, r); err != nil {
		f.t.Fatal(err)
	}
	f.d.flush(context.Background())
}

type sent struct {
	event, token string
	state        State
	aps          map[string]json.RawMessage
}

func (f *fixture) last() sent {
	f.t.Helper()
	if len(f.sender.Calls) == 0 {
		f.t.Fatal("nothing pushed")
	}
	req := f.sender.Calls[len(f.sender.Calls)-1]
	if req.PushType != relay.PushTypeLiveActivity || req.Priority != 10 {
		f.t.Fatalf("push type %q priority %d", req.PushType, req.Priority)
	}
	var body struct {
		APS map[string]json.RawMessage `json:"aps"`
	}
	if err := json.Unmarshal(req.Payload, &body); err != nil {
		f.t.Fatal(err)
	}
	s := sent{token: req.Token, aps: body.APS}
	_ = json.Unmarshal(body.APS["event"], &s.event)
	_ = json.Unmarshal(body.APS["content-state"], &s.state)
	return s
}

// deployAt is the deploy's whole state at a step, as a plugin sends it.
func deployAt(current int, body, status string) Request {
	return Request{ID: "deploy:web", Project: "/Users/me/web", Content: Content{
		Title: "Deploying web", Body: body, Symbol: "shippingbox", Page: ".repogo/tabs/deploys.ui",
		Progress: &Progress{Kind: ProgressSteps, Steps: []string{"Build", "Test", "Deploy", "Verify"}, Current: current},
		Status:   status,
	}}
}

var deploy = deployAt(0, "Building", "")

func titled(id, title string) Request {
	return Request{ID: id, Content: Content{Title: title}}
}

func TestFirstSetStartsTheActivity(t *testing.T) {
	f := setup(t)
	f.set("vercel", deploy)
	s := f.last()
	if s.event != "start" || s.token != "57a4" {
		t.Fatalf("event %q to %q", s.event, s.token)
	}
	if s.state.Title != "Deploying web" || s.state.Body != "Building" || len(s.state.Progress.Steps) != 4 ||
		s.state.Symbol != "shippingbox" || s.state.Page != ".repogo/tabs/deploys.ui" || s.state.Status != Running {
		t.Errorf("state %+v", s.state)
	}
	if *s.state.Timer != (Timer{Kind: TimerElapsed, StartedAt: f.now.UnixMilli()}) {
		t.Errorf("timer %+v", *s.state.Timer)
	}
	var attrs Attributes
	_ = json.Unmarshal(s.aps["attributes"], &attrs)
	if attrs != (Attributes{HostID: string(f.devices.Identity().ID), Key: "vercel/deploy:web", Project: "/Users/me/web"}) {
		t.Errorf("attributes %+v", attrs)
	}
	var kind string
	_ = json.Unmarshal(s.aps["attributes-type"], &kind)
	if kind != "StepActivityAttributes" {
		t.Errorf("attributes-type %q", kind)
	}
	if _, ok := s.aps["input-push-token"]; !ok {
		t.Error("start asks for no update token")
	}
}

// Each call is the whole state: what it leaves out takes its default, and
// an empty label stays empty, which hides it.
func TestEachSetIsTheWholeState(t *testing.T) {
	f := setup(t)
	f.set("vercel", deploy)
	f.register(device.PushSteps, "vercel/deploy:web", "a1")
	started := f.last().state.Timer.StartedAt

	hidden := ""
	next := titled("deploy:web", "Deploying web")
	next.Label = &hidden
	f.now = f.now.Add(updateInterval)
	f.set("vercel", next)
	s := f.last()
	if s.state.Symbol != "" || s.state.Progress != nil || s.state.Label == nil || *s.state.Label != "" {
		t.Errorf("state %+v", s.state)
	}
	if s.state.Timer.StartedAt != started {
		t.Errorf("timer restarted: %d, want %d", s.state.Timer.StartedAt, started)
	}
}

func TestAPluginSetsTheEnding(t *testing.T) {
	f := setup(t)
	f.set("vercel", deploy)
	f.register(device.PushSteps, "vercel/deploy:web", "a1")

	label, ends := "Live", f.now.Add(time.Minute).UnixMilli()
	done := deployAt(4, "web.app", Done)
	done.Symbol, done.Tint, done.Label = "globe", "blue", &label
	done.Timer = &Timer{Kind: TimerCountdown, EndsAt: ends}
	f.now = f.now.Add(minGap)
	f.set("vercel", done)
	s := f.last()
	if s.event != "end" || s.state.Symbol != "globe" || s.state.Tint != "blue" || *s.state.Label != "Live" ||
		*s.state.Timer != (Timer{Kind: TimerCountdown, EndsAt: ends}) {
		t.Errorf("event %q, state %+v", s.event, s.state)
	}
}

// Apple takes at most 4 KB, which escaping can pass inside every field's limit.
func TestAPushPastFourKilobytesIsRefused(t *testing.T) {
	f := setup(t)
	r := titled("a", "t")
	r.Page, r.Project = strings.Repeat("<", maxPage), strings.Repeat("<", maxProject)
	if err := f.d.Set("p", r); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("set = %v", err)
	}
	if len(f.d.live) != 0 {
		t.Error("kept the refused activity")
	}
}

func TestUpdatesWaitForTheInterval(t *testing.T) {
	f := setup(t)
	f.set("vercel", deploy)
	f.register(device.PushSteps, "vercel/deploy:web", "a1")

	next := deployAt(1, "Running tests", "")
	if err := f.d.Set("vercel", next); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	if at := f.d.flush(context.Background()); len(f.sender.Calls) != 1 || at.IsZero() {
		t.Fatalf("pushed %d before the interval, next %v", len(f.sender.Calls), at)
	}
	f.now = f.now.Add(updateInterval)
	f.d.flush(context.Background())
	s := f.last()
	if s.event != "update" || s.token != "a1" || s.state.Progress.Current != 1 || s.state.Body != "Running tests" {
		t.Errorf("event %q to %q, state %+v", s.event, s.token, s.state)
	}
}

func TestDoneEndsAndForgets(t *testing.T) {
	f := setup(t)
	f.set("vercel", deploy)
	f.register(device.PushSteps, "vercel/deploy:web", "a1")

	done := deployAt(3, "Live", Done)
	f.now = f.now.Add(minGap)
	f.set("vercel", done)
	s := f.last()
	if s.event != "end" || s.token != "a1" || s.state.Status != Done || s.state.EndedAt != f.now.UnixMilli() {
		t.Fatalf("event %q to %q, state %+v", s.event, s.token, s.state)
	}
	var dismissal int64
	_ = json.Unmarshal(s.aps["dismissal-date"], &dismissal)
	if dismissal != f.now.Add(endedFor).Unix() {
		t.Errorf("dismissal %d", dismissal)
	}
	if p, _ := f.devices.Peer(f.phone.ID); len(p.Steps) != 0 {
		t.Errorf("token kept: %+v", p.Steps)
	}
	if len(f.d.live) != 0 {
		t.Error("activity still live")
	}
}

func TestEndingWhatIsNotLiveDoesNothing(t *testing.T) {
	f := setup(t)
	done := deployAt(0, "", Done)
	f.set("vercel", done)
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
	_, _ = s.Sender.Control(ctx, method, params)
	if params.(relay.PushRequest).Token == s.token {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeNotFound, Message: "Unregistered"}
	}
	return nil, nil
}

// The user swiped it away: its token goes and it never starts there again.
func TestDismissedStaysDismissed(t *testing.T) {
	f := setup(t)
	sender := &deadFor{token: "a1"}
	f.d.send = sender
	f.set("vercel", deploy)
	f.register(device.PushSteps, "vercel/deploy:web", "a1")

	f.now = f.now.Add(updateInterval)
	f.set("vercel", deployAt(1, "", ""))
	f.now = f.now.Add(startRetry + updateInterval)
	f.set("vercel", deployAt(2, "", ""))

	if p, _ := f.devices.Peer(f.phone.ID); len(p.Steps) != 0 || p.StepsStart == nil {
		t.Errorf("steps %+v, start %v", p.Steps, p.StepsStart)
	}
	if calls := len(sender.Calls); calls != 2 {
		t.Errorf("pushed %d, want the start and the dead update", calls)
	}
}

func TestOneStartUntilTheTokenArrives(t *testing.T) {
	f := setup(t)
	f.set("vercel", deploy)
	f.now = f.now.Add(updateInterval)
	f.set("vercel", deployAt(1, "", ""))
	if len(f.sender.Calls) != 1 {
		t.Fatalf("pushed %d", len(f.sender.Calls))
	}
	f.now = f.now.Add(startRetry)
	f.d.TokensChanged()
	f.d.flush(context.Background())
	if s := f.last(); len(f.sender.Calls) != 2 || s.event != "start" || s.state.Progress.Current != 1 {
		t.Errorf("pushed %d, last %q %+v", len(f.sender.Calls), s.event, s.state)
	}
}

func TestFailedPushIsRetried(t *testing.T) {
	f := setup(t)
	f.sender.Reply = errors.New("relay down")
	f.set("vercel", deploy)
	f.sender.Reply = nil
	f.now = f.now.Add(updateInterval)
	f.d.flush(context.Background())
	if len(f.sender.Calls) != 2 || f.last().event != "start" {
		t.Errorf("pushed %d", len(f.sender.Calls))
	}
}

func TestAPluginShowsTwoAtOnce(t *testing.T) {
	f := setup(t)
	for _, id := range []string{"a", "b"} {
		if err := f.d.Set("p", titled(id, id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.d.Set("p", titled("c", "c")); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("third = %v", err)
	}
	if err := f.d.Set("other", titled("c", "c")); err != nil {
		t.Errorf("another plugin's = %v", err)
	}
	if err := f.d.Set("p", titled("a", "a again")); err != nil {
		t.Errorf("updating one = %v", err)
	}
	// One left without updates stops counting.
	f.now = f.now.Add(idleAfter)
	if err := f.d.Set("p", titled("c", "c")); err != nil {
		t.Errorf("after the idle ones = %v", err)
	}
}

func TestActivityIsForgottenWhenIOSEndsIt(t *testing.T) {
	f := setup(t)
	f.set("vercel", deploy)
	f.now = f.now.Add(maxAge)
	f.d.flush(context.Background())
	if len(f.d.live) != 0 {
		t.Error("kept past eight hours")
	}
}

func TestInvalidRequests(t *testing.T) {
	f := setup(t)
	steps := func(current int, names ...string) *Progress {
		return &Progress{Kind: ProgressSteps, Steps: names, Current: current}
	}
	long := strings.Repeat("x", maxLabel+1)
	for name, c := range map[string]Content{
		"no title":        {},
		"too many steps":  {Title: "t", Progress: steps(0, "1", "2", "3", "4", "5", "6", "7", "8", "9")},
		"no steps":        {Title: "t", Progress: steps(0)},
		"current":         {Title: "t", Progress: steps(3, "1", "2")},
		"empty step":      {Title: "t", Progress: steps(0, "")},
		"value on steps":  {Title: "t", Progress: &Progress{Kind: ProgressSteps, Steps: []string{"1"}, Value: 0.5}},
		"bar value":       {Title: "t", Progress: &Progress{Kind: ProgressBar, Value: 1.5}},
		"steps on a bar":  {Title: "t", Progress: &Progress{Kind: ProgressBar, Steps: []string{"1"}}},
		"progress kind":   {Title: "t", Progress: &Progress{Kind: "ring"}},
		"countdown":       {Title: "t", Timer: &Timer{Kind: TimerCountdown}},
		"none with times": {Title: "t", Timer: &Timer{Kind: TimerNone, EndsAt: 1}},
		"timer kind":      {Title: "t", Timer: &Timer{Kind: "lap"}},
		"status":          {Title: "t", Status: "paused"},
		"symbol":          {Title: "t", Symbol: "../x"},
		"tint":            {Title: "t", Tint: "secondary"},
		"label":           {Title: "t", Label: &long},
	} {
		if err := f.d.Set("p", Request{ID: "a", Content: c}); !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("%s = %v", name, err)
		}
	}
	if err := f.d.Set("p", Request{Content: Content{Title: "t"}}); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("no id = %v", err)
	}
}
func TestRunPushesNothingAfterShutdown(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.d.Set("vercel", deploy); err != nil {
		t.Fatal(err)
	}
	f.d.flush(ctx)
	if len(f.sender.Calls) != 0 {
		t.Errorf("pushed %d", len(f.sender.Calls))
	}
}
