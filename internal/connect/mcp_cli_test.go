package connect

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/clilogin"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/mcp"
)

// fakeMCP answers as the MCP service does, keeping what each call was given.
type fakeMCP struct {
	servers  []mcp.Server
	auth     mcp.AuthKind
	manual   bool
	key      string // the API key Connect was given
	clientID string
	loopback bool
	finished []string // codes OAuthFinish exchanged
	enabled  []string
	connects int
	// entered and gate, when set, hold Connect until the test lets it go.
	entered chan struct{}
	gate    chan struct{}
}

func (m *fakeMCP) List(string) mcp.Listing { return mcp.Listing{Servers: m.servers} }

func (m *fakeMCP) Probe(_ context.Context, url, label, project string) (mcp.Probe, error) {
	return mcp.Probe{ProbeID: "probe", URL: url, Auth: m.auth, NeedsManualClient: m.manual}, nil
}

func (m *fakeMCP) Connect(probeID, apiKey string) (mcp.Server, error) {
	m.connects++
	if m.gate != nil {
		m.entered <- struct{}{}
		<-m.gate
	}
	if m.auth == mcp.AuthAPIKey && apiKey == "" {
		return mcp.Server{}, errkind.ErrInvalid.Errorf("this server needs an API key")
	}
	m.key = apiKey
	return mcp.Server{ID: "linear"}, nil
}

func (m *fakeMCP) OAuthStart(_ context.Context, probeID, clientID string, loopback bool) (string, string, error) {
	m.clientID, m.loopback = clientID, loopback
	return "https://linear.app/oauth/authorize?state=st", "st", nil
}

func (m *fakeMCP) OAuthFinish(_ context.Context, probeID, code, state string) (mcp.Server, error) {
	m.finished = append(m.finished, code)
	if code == "bad" {
		return mcp.Server{}, errors.New("the token endpoint refused the code")
	}
	return mcp.Server{ID: "linear"}, nil
}

func (m *fakeMCP) SetEnabled(id, project string, enabled bool) error {
	m.enabled = append(m.enabled, id+" "+project)
	return nil
}

// fakeCLIs is the CLIs on a machine: installs by kind, and the sign-in each starts.
type fakeCLIs struct {
	installs  map[string]clitool.Install
	code      clilogin.Code
	pasted    string
	cancelled []string
	// started runs inside LoginStart, as a CLI that ends at once would.
	started func()
	// refuse is what LoginComplete answers, as a CLI's last line.
	refuse error
}

func (c *fakeCLIs) Get(_ context.Context, kind string) (clitool.Install, bool) {
	i, ok := c.installs[kind]
	return i, ok
}

func (c *fakeCLIs) LoginStart(context.Context, string) (clilogin.Code, error) {
	if c.started != nil {
		c.started()
	}
	return c.code, nil
}

func (c *fakeCLIs) LoginComplete(_ context.Context, kind, code string) error {
	c.pasted = code
	return c.refuse
}

func (c *fakeCLIs) LoginCancel(kind string) { c.cancelled = append(c.cancelled, kind) }

const linear = "mcp:https://mcp.linear.app/mcp"

func (f *fixture) begin(t *testing.T, p StartParams) Flow {
	t.Helper()
	p.Project = f.project
	flow, err := f.s.Start(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

func TestMCPAPIKey(t *testing.T) {
	f := newFixture(t)
	f.mcp.auth = mcp.AuthAPIKey
	flow := f.begin(t, StartParams{Target: linear})
	if flow.Step == nil || flow.Step.Kind != StepSecret || flow.Step.Label != "API key" {
		t.Fatalf("start = %+v", flow)
	}
	if _, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "  "); err == nil {
		t.Fatal("an empty key was taken")
	}
	// The MCP service spends its probe on any call, so an empty key never reaches it.
	if f.mcp.connects != 0 {
		t.Fatalf("Connect ran %d times for an empty key", f.mcp.connects)
	}
	done, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "lin_api_x")
	if err != nil || done.State != Succeeded || f.mcp.key != "lin_api_x" {
		t.Fatalf("submit = %+v, %v; key %q", done, err, f.mcp.key)
	}
}

func TestMCPOAuthThroughTheCallback(t *testing.T) {
	f := newFixture(t)
	f.mcp.auth = mcp.AuthOAuth
	flow := f.begin(t, StartParams{Target: linear, Loopback: true})
	step := flow.Step
	if step == nil || step.Kind != StepBrowser || step.Redirect != RedirectLoopback || step.URL == "" || !f.mcp.loopback {
		t.Fatalf("start = %+v", flow)
	}
	if _, err := f.s.Callback(ctx, flow.ID, "other", "code", ""); err == nil {
		t.Fatal("a redirect with another sign-in's state was taken")
	}
	if got, _ := f.s.Get(flow.ID); got.State != Waiting {
		t.Fatalf("a stray redirect ended the sign-in: %+v", got)
	}
	if _, err := f.s.Callback(ctx, "missing", "st", "code", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown flow: %v", err)
	}
	done, err := f.s.Callback(ctx, flow.ID, "st", "good", "")
	if err != nil || done.State != Succeeded {
		t.Fatalf("callback = %+v, %v", done, err)
	}
	if _, err := f.s.Callback(ctx, flow.ID, "st", "good", ""); err == nil {
		t.Fatal("a second callback was taken")
	}
	if len(f.mcp.finished) != 1 {
		t.Fatalf("exchanged %v", f.mcp.finished)
	}
}

func TestMCPOAuthFailures(t *testing.T) {
	f := newFixture(t)
	f.mcp.auth = mcp.AuthOAuth
	declined := f.begin(t, StartParams{Target: linear})
	got, _ := f.s.Callback(ctx, declined.ID, "st", "", "access_denied")
	if got.State != Failed || !strings.Contains(got.Reason, "access_denied") {
		t.Fatalf("declined = %+v", got)
	}
	// The provider's own words never become the reason.
	forged := f.begin(t, StartParams{Target: linear})
	if got, _ := f.s.Callback(ctx, forged.ID, "st", "", "token sk_live_123 leaked"); strings.Contains(got.Reason, "sk_live") {
		t.Fatalf("provider text in the reason: %q", got.Reason)
	}
	if len(f.mcp.finished) != 0 {
		t.Fatal("a declined sign-in was exchanged")
	}

	refused := f.begin(t, StartParams{Target: linear})
	if got, _ := f.s.Callback(ctx, refused.ID, "st", "bad", ""); got.ID != refused.ID || got.State != Failed {
		t.Fatalf("refused = %+v", got)
	}
}

func TestMCPClientIDThenSignIn(t *testing.T) {
	f := newFixture(t)
	f.mcp.auth, f.mcp.manual = mcp.AuthOAuth, true
	flow := f.begin(t, StartParams{Target: linear})
	if flow.Step == nil || flow.Step.ID != "client_id" || flow.Step.Kind != StepPaste {
		t.Fatalf("start = %+v", flow)
	}
	next, err := f.s.Submit(ctx, flow.ID, "client_id", " my-client ")
	if err != nil || next.State != Waiting || next.Step == nil || next.Step.Kind != StepBrowser || f.mcp.clientID != "my-client" {
		t.Fatalf("submit = %+v, %v", next, err)
	}
	if got, _ := f.s.Callback(ctx, flow.ID, "st", "good", ""); got.State != Succeeded {
		t.Fatalf("callback = %+v", got)
	}
}

func TestMCPWithNothingToAsk(t *testing.T) {
	f := newFixture(t)
	f.mcp.auth = mcp.AuthNone
	if flow := f.begin(t, StartParams{Target: linear}); flow.State != Succeeded || flow.Step != nil {
		t.Fatalf("none = %+v", flow)
	}

	// Connected elsewhere: starting switches it on here, with no sign-in.
	f.mcp.servers = []mcp.Server{{ID: "lin", Label: "Linear", URL: "https://mcp.linear.app/mcp", Projects: []string{"/other"}}}
	if got := f.s.List(ctx, f.project, []Want{{Target: linear}}); got[0].Status != Missing || got[0].Destination != "Linear" {
		t.Fatalf("off here = %+v", got)
	}
	if flow := f.begin(t, StartParams{Target: linear}); flow.State != Succeeded || len(f.mcp.enabled) != 1 {
		t.Fatalf("switch on = %+v, %v", flow, f.mcp.enabled)
	}
	f.mcp.servers[0].Projects = append(f.mcp.servers[0].Projects, f.project)
	if got := f.s.List(ctx, f.project, []Want{{Target: "mcp:mcp.linear.app/mcp"}}); got[0].Status != Connected {
		t.Fatalf("on here = %+v", got)
	}
}

func TestCLIDeviceSignInEndsWithTheCLI(t *testing.T) {
	f := newFixture(t)
	f.clis.installs["gh"] = clitool.Install{Name: "GitHub CLI", Installed: true}
	f.clis.code = clilogin.Code{URL: "https://github.com/login/device", Style: clilogin.StyleDevice, UserCode: "ABCD-1234"}
	flow := f.begin(t, StartParams{Target: "cli:gh"})
	if flow.Step == nil || flow.Step.Kind != StepBrowser || flow.Step.UserCode != "ABCD-1234" || flow.Step.Redirect != RedirectNone {
		t.Fatalf("start = %+v", flow)
	}
	f.s.CLIEnded("codex", true)
	if got, _ := f.s.Get(flow.ID); got.State != Waiting {
		t.Fatalf("another CLI's end moved it: %+v", got)
	}
	f.s.CLIEnded("gh", true)
	if got, _ := f.s.Get(flow.ID); got.State != Succeeded {
		t.Fatalf("after = %+v", got)
	}
}

func TestCLIPasteAndCallbackSteps(t *testing.T) {
	f := newFixture(t)
	f.clis.installs["claude"] = clitool.Install{Name: "Claude Code", Installed: true}
	f.clis.code = clilogin.Code{URL: "https://claude.ai/oauth", Style: clilogin.StylePaste}
	flow := f.begin(t, StartParams{Target: "cli:claude"})
	if flow.Step == nil || flow.Step.Kind != StepPaste || flow.Step.URL == "" {
		t.Fatalf("paste = %+v", flow)
	}
	if _, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "code#123"); err != nil || f.clis.pasted != "code#123" {
		t.Fatalf("submit: %v, pasted %q", err, f.clis.pasted)
	}

	f.clis.installs["wrangler"] = clitool.Install{Name: "Wrangler", Installed: true}
	f.clis.code = clilogin.Code{URL: "https://dash.cloudflare.com/oauth2/auth", Style: clilogin.StyleCallback}
	if flow := f.begin(t, StartParams{Target: "cli:wrangler"}); flow.Step.Redirect != RedirectLocalhost {
		t.Fatalf("callback = %+v", flow)
	}
}

func TestCLIStatusAndWhatCantStart(t *testing.T) {
	f := newFixture(t)
	f.clis.installs["gh"] = clitool.Install{Name: "GitHub CLI", Installed: true, Authed: true,
		Account: &clitool.Account{Login: "octocat"}}
	f.clis.installs["vercel"] = clitool.Install{Name: "Vercel CLI"}
	got := f.s.List(ctx, f.project, []Want{{Target: "cli:gh"}, {Target: "cli:vercel"}, {Target: "cli:nope"}})
	if got[0].Status != Connected || got[0].Account != "octocat" {
		t.Fatalf("gh = %+v", got[0])
	}
	if got[1].Status != Unavailable || got[1].Reason != "Install Vercel CLI on this environment first" || got[2].Status != Unavailable {
		t.Fatalf("vercel, nope = %+v, %+v", got[1], got[2])
	}
	if flow := f.begin(t, StartParams{Target: "cli:gh"}); flow.State != Succeeded {
		t.Fatalf("signed in already = %+v", flow)
	}
	if _, err := f.s.Start(ctx, StartParams{Project: f.project, Target: "cli:vercel"}); err == nil {
		t.Fatal("started a sign-in for a CLI that isn't installed")
	}
}

func TestAnExpiredCLISignInIsStopped(t *testing.T) {
	f := newFixture(t)
	f.clis.installs["gh"] = clitool.Install{Name: "GitHub CLI", Installed: true}
	flow := f.begin(t, StartParams{Target: "cli:gh"})
	f.now = f.now.Add(waitFor)
	if got, _ := f.s.Get(flow.ID); got.State != Expired {
		t.Fatalf("state = %s", got.State)
	}
	if len(f.clis.cancelled) != 1 || f.clis.cancelled[0] != "gh" {
		t.Fatalf("cancelled = %v", f.clis.cancelled)
	}
}

func TestASecondSubmitWaitsForTheFirst(t *testing.T) {
	f := newFixture(t)
	f.mcp.auth = mcp.AuthAPIKey
	f.mcp.entered, f.mcp.gate = make(chan struct{}), make(chan struct{})
	flow := f.begin(t, StartParams{Target: linear})

	first := make(chan Flow)
	go func() {
		done, _ := f.s.Submit(ctx, flow.ID, flow.Step.ID, "key-1")
		first <- done
	}()
	<-f.mcp.entered
	if _, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "key-2"); err == nil {
		t.Fatal("a second submit ran while the first was saving")
	}
	if err := f.s.Cancel(flow.ID); err == nil {
		t.Fatal("cancelled while saving")
	}
	close(f.mcp.gate)
	if done := <-first; done.State != Succeeded || f.mcp.key != "key-1" || f.mcp.connects != 1 {
		t.Fatalf("first = %+v; key %q after %d connects", done, f.mcp.key, f.mcp.connects)
	}
}

func TestACLIThatEndsAtOnceEndsItsFlow(t *testing.T) {
	f := newFixture(t)
	f.clis.installs["github"] = clitool.Install{Name: "GitHub", Installed: true}
	f.clis.started = func() { f.s.CLIEnded("github", true) }
	if flow := f.begin(t, StartParams{Target: "cli:github"}); flow.State != Succeeded || flow.Step != nil {
		t.Fatalf("flow = %+v", flow)
	}
}

func TestOneSheetDoesntCancelAnothersSignIn(t *testing.T) {
	f := newFixture(t)
	f.clis.installs["github"] = clitool.Install{Name: "GitHub", Installed: true}
	a := f.begin(t, StartParams{Target: "cli:github"})
	b := f.begin(t, StartParams{Target: "cli:github"})
	if err := f.s.Cancel(a.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.clis.cancelled) != 0 {
		t.Fatalf("cancelled %v while another sheet waits", f.clis.cancelled)
	}
	if err := f.s.Cancel(b.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.clis.cancelled) != 1 {
		t.Fatalf("cancelled = %v", f.clis.cancelled)
	}
}

func TestACLIsOutputIsNeverTheReason(t *testing.T) {
	f := newFixture(t)
	f.clis.installs["claude"] = clitool.Install{Name: "Claude", Installed: true}
	f.clis.code = clilogin.Code{URL: "https://claude.ai/oauth", Style: clilogin.StylePaste}
	f.clis.refuse = errors.New("invalid code: pasted-secret-123")
	flow := f.begin(t, StartParams{Target: "cli:claude"})
	if _, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, " "); err == nil {
		t.Fatal("an empty code was taken")
	}
	done, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "pasted-secret-123")
	if err != nil || done.State != Failed || strings.Contains(done.Reason, "pasted-secret") {
		t.Fatalf("submit = %+v, %v", done, err)
	}
}
