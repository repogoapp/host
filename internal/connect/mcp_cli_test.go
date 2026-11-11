package connect

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	another  int      // ProbeAnother calls
	again    []string // connections ProbeAgain signed in again
	// refuseStart has OAuthStart fail, as a server refusing registration does.
	refuseStart bool
	// entered and gate, when set, hold Connect until the test lets it go.
	entered chan struct{}
	gate    chan struct{}
}

func (m *fakeMCP) List(string) mcp.Listing { return mcp.Listing{Servers: m.servers} }

func (m *fakeMCP) Probe(_ context.Context, url, label, project string) (mcp.Probe, error) {
	return mcp.Probe{ProbeID: "probe", URL: url, Auth: m.auth, NeedsManualClient: m.manual}, nil
}

func (m *fakeMCP) ProbeAgain(ctx context.Context, id, project string) (mcp.Probe, error) {
	m.again = append(m.again, id)
	return m.Probe(ctx, "", "", project)
}

func (m *fakeMCP) ProbeAnother(ctx context.Context, url, label, project string) (mcp.Probe, error) {
	m.another++
	return m.Probe(ctx, url, label, project)
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
	if m.refuseStart && clientID == "" {
		return "", "", errkind.ErrInvalid.Errorf("this server could not register RepoGo")
	}
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
	m.enabled = append(m.enabled, fmt.Sprintf("%s %s %t", id, project, enabled))
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
	// loggedOut is each CLI Logout was asked to sign out.
	loggedOut []string
	// accountLogins, selected, renamed and discarded are each account
	// sign-in, pick, rename and removal.
	accountLogins, selected, renamed, discarded []string
	// picks has PickedAccount answer, for a project with an account picked;
	// pickedOut has that account signed out, pickGone the CLI no longer have it.
	picks, pickedOut, pickGone bool
	// selectErr is what SelectAccount answers.
	selectErr error
}

func (c *fakeCLIs) Logout(_ context.Context, kind string) (clitool.UpdateResult, error) {
	c.loggedOut = append(c.loggedOut, kind)
	return clitool.UpdateResult{Kind: kind, OK: true}, nil
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

// AccountLoginStart signs in an account as LoginStart does, keeping which.
func (c *fakeCLIs) AccountLoginStart(_ context.Context, kind, id string, add bool) (clilogin.Code, error) {
	if id == "bad name" {
		return clilogin.Code{}, errkind.ErrInvalid.Errorf("name it with letters")
	}
	c.accountLogins = append(c.accountLogins, fmt.Sprintf("%s %s %t", kind, id, add))
	return c.code, nil
}

func (c *fakeCLIs) SelectAccount(_ context.Context, kind, root, id string) error {
	if c.selectErr != nil {
		return c.selectErr
	}
	c.selected = append(c.selected, kind+" "+id)
	return nil
}

func (c *fakeCLIs) AccountWho(context.Context, string, string) (string, string) {
	return "976193265892 · root", "fileie"
}

func (c *fakeCLIs) RenameAccount(_ context.Context, kind, from, to string) error {
	if to == "bad name" {
		return errkind.ErrInvalid.Errorf("name it with letters")
	}
	c.renamed = append(c.renamed, from+" "+to)
	return nil
}

// PickedAccount: "client-a" is picked everywhere.
func (c *fakeCLIs) PickedAccount(_ context.Context, kind, root string) (string, clitool.CLIAccount, bool) {
	switch {
	case !c.picks:
		return "", clitool.CLIAccount{}, false
	case c.pickGone:
		return "client-a", clitool.CLIAccount{}, false
	}
	return "client-a", clitool.CLIAccount{ID: "client-a", Detail: "client-a@example.com", SignedIn: !c.pickedOut, On: true}, true
}

func (c *fakeCLIs) DiscardAccount(_ context.Context, kind, id string) error {
	c.discarded = append(c.discarded, id)
	return nil
}

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

// A caller that asks to confirm gets a confirm step for a server with no
// sign-in, and it connects on the submit; one that doesn't connects at once.
func TestMCPWithNothingToAskWaitsWhenAskedTo(t *testing.T) {
	f := newFixture(t)
	f.mcp.auth = mcp.AuthNone
	flow := f.begin(t, StartParams{Target: linear, Confirm: true})
	if flow.State != Waiting || flow.Step == nil || flow.Step.Kind != StepConfirm || f.mcp.connects != 0 {
		t.Fatalf("confirm = %+v after %d connects", flow, f.mcp.connects)
	}
	done, err := f.s.Submit(ctx, flow.ID, "confirm", "")
	if err != nil || done.State != Succeeded || f.mcp.connects != 1 {
		t.Fatalf("submit = %+v, %v after %d connects", done, err, f.mcp.connects)
	}
	if flow := f.begin(t, StartParams{Target: linear}); flow.State != Succeeded || f.mcp.connects != 2 {
		t.Fatalf("without confirm = %+v", flow)
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

// A plain Connect in a project with an account picked signs that account in,
// not the CLI's own; a pick the CLI no longer has says so, and signs nothing.
func TestAPlainConnectSignsInThePick(t *testing.T) {
	f := newFixture(t)
	f.clis.installs["aws"] = clitool.Install{Name: "AWS", Installed: true, Authed: true}
	f.clis.code = clilogin.Code{URL: "https://us-east-1.signin.aws.amazon.com/v1/authorize", Style: clilogin.StylePaste}
	f.clis.picks, f.clis.pickedOut = true, true
	flow := f.begin(t, StartParams{Target: "cli:aws"})
	if flow.State != Waiting || !slices.Equal(f.clis.accountLogins, []string{"aws client-a false"}) {
		t.Fatalf("plain connect = %+v, account sign-ins %v", flow, f.clis.accountLogins)
	}

	f.clis.pickGone = true
	got := f.s.List(ctx, f.project, []Want{{Target: "cli:aws"}})
	if got[0].Status != Missing || !strings.Contains(got[0].Reason, "client-a") {
		t.Fatalf("gone pick = %+v", got[0])
	}
	if _, err := f.s.Start(ctx, StartParams{Target: "cli:aws", Project: f.project}); err == nil {
		t.Fatal("a gone pick started a sign-in")
	}
}

// A new account the project couldn't switch to is an error on its name
// step, not a success; trying again picks it without renaming it twice.
func TestAFailedPickIsAnError(t *testing.T) {
	f := newFixture(t)
	f.clis.installs["aws"] = clitool.Install{Name: "AWS", Installed: true, Authed: true}
	f.clis.code = clilogin.Code{URL: "https://us-east-1.signin.aws.amazon.com/v1/authorize", Style: clilogin.StylePaste}
	add := f.begin(t, StartParams{Target: "cli:aws", Account: "another"})
	if _, err := f.s.Submit(ctx, add.ID, "code", "the-code"); err != nil {
		t.Fatal(err)
	}
	f.clis.selectErr = errkind.ErrInvalid.Errorf("there's no account called fileie")
	if _, err := f.s.Submit(ctx, add.ID, stepAccountName, "fileie"); err == nil || !strings.Contains(err.Error(), "couldn't switch") {
		t.Fatalf("failed pick = %v", err)
	}
	if got, _ := f.s.Get(add.ID); got.State != Waiting {
		t.Fatalf("after a failed pick = %+v", got)
	}
	f.clis.selectErr = nil
	done, err := f.s.Submit(ctx, add.ID, stepAccountName, "fileie")
	if err != nil || done.State != Succeeded || len(f.clis.renamed) != 1 || !slices.Equal(f.clis.selected, []string{"aws fileie"}) {
		t.Fatalf("retry = %+v, %v, renamed %v, selected %v", done, err, f.clis.renamed, f.clis.selected)
	}
}

// A project with an account picked shows that account; one without, the
// CLI's own login on this environment.
func TestACLIShowsTheProjectsPickedAccount(t *testing.T) {
	f := newFixture(t)
	f.clis.installs["fly"] = clitool.Install{Name: "Fly", Installed: true, Authed: true,
		Account: &clitool.Account{Email: "own@example.com"}}
	got := f.s.List(ctx, f.project, []Want{{Target: "cli:fly"}})
	if got[0].Status != Connected || got[0].Account != "own@example.com" {
		t.Fatalf("no pick = %+v", got[0])
	}
	f.clis.picks = true
	got = f.s.List(ctx, f.project, []Want{{Target: "cli:fly"}})
	if got[0].Status != Connected || got[0].Account != "client-a@example.com" {
		t.Fatalf("picked = %+v", got[0])
	}
}

// A CLI with several accounts signs one in again by id, or adds one: signed
// in first under a name of its own, then named once it's clear which account
// it is, and its project uses it. One whose sign-in fails is discarded.
func TestCLIAccountsSignInAndAdd(t *testing.T) {
	f := newFixture(t)
	f.clis.installs["aws"] = clitool.Install{Name: "AWS", Installed: true, Authed: true}
	f.clis.code = clilogin.Code{URL: "https://us-east-1.signin.aws.amazon.com/v1/authorize", Style: clilogin.StylePaste}

	again := f.begin(t, StartParams{Target: "cli:aws", Account: "again:client-a"})
	if again.State != Waiting || again.Step == nil || again.Step.ID != "code" {
		t.Fatalf("again = %+v", again)
	}
	f.s.CLIEnded("aws", true)

	add := f.begin(t, StartParams{Target: "cli:aws", Account: "another"})
	if add.Step == nil || add.Step.ID != "code" || len(f.clis.accountLogins) != 2 {
		t.Fatalf("add = %+v, %v", add.Step, f.clis.accountLogins)
	}
	temp := strings.Fields(f.clis.accountLogins[1])[1]
	if !strings.HasPrefix(temp, "new-") || !strings.HasSuffix(f.clis.accountLogins[1], " true") {
		t.Fatalf("add signed in as %q", f.clis.accountLogins[1])
	}
	named, err := f.s.Submit(ctx, add.ID, "code", "the-code")
	if err != nil || named.Step == nil || named.Step.ID != stepAccountName || named.Step.Value != "fileie" ||
		!strings.Contains(named.Step.Note, "976193265892") {
		t.Fatalf("after the code = %+v, %v", named.Step, err)
	}
	if _, err := f.s.Submit(ctx, add.ID, stepAccountName, "bad name"); errkind.Of(err) != errkind.Invalid {
		t.Fatalf("bad name = %v", err)
	}
	done, err := f.s.Submit(ctx, add.ID, stepAccountName, " fileie ")
	if err != nil || done.State != Succeeded {
		t.Fatalf("named = %+v, %v", done, err)
	}
	if want := []string{temp + " fileie"}; !slices.Equal(f.clis.renamed, want) {
		t.Fatalf("renamed = %v", f.clis.renamed)
	}
	if want := []string{"aws fileie"}; !slices.Equal(f.clis.selected, want) {
		t.Fatalf("selected = %v", f.clis.selected)
	}

	failed := f.begin(t, StartParams{Target: "cli:aws", Account: "another"})
	f.s.CLIEnded("aws", false)
	if got, _ := f.s.Get(failed.ID); got.State != Failed || len(f.clis.discarded) != 1 {
		t.Fatalf("failed add = %+v, discarded %v", got, f.clis.discarded)
	}

	// Closed before it signed in: its account goes too.
	closed := f.begin(t, StartParams{Target: "cli:aws", Account: "another"})
	if err := f.s.Cancel(closed.ID); err != nil || len(f.clis.discarded) != 2 {
		t.Fatalf("closed add: %v, discarded %v", err, f.clis.discarded)
	}
	// Closed at its name, once signed in: its account stays, to rename.
	atName := f.begin(t, StartParams{Target: "cli:aws", Account: "another"})
	if _, err := f.s.Submit(ctx, atName.ID, "code", "the-code"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Cancel(atName.ID); err != nil || len(f.clis.discarded) != 2 {
		t.Fatalf("closed at its name: %v, discarded %v", err, f.clis.discarded)
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

func TestDisconnectSignsACLIOutAndSwitchesAServerOffHere(t *testing.T) {
	f := newFixture(t)
	f.clis.installs["github"] = clitool.Install{Name: "GitHub", Installed: true, Authed: true}
	cli := f.begin(t, StartParams{Target: "cli:github"})
	if done, err := f.s.Disconnect(ctx, cli.ID); err != nil || done.State != Disconnected || len(f.clis.loggedOut) != 1 {
		t.Fatalf("cli = %+v, %v; logged out %v", done, err, f.clis.loggedOut)
	}

	f.mcp.servers = []mcp.Server{{ID: "lin", Label: "Linear", URL: "https://mcp.linear.app/mcp", Projects: []string{f.project}}}
	server := f.begin(t, StartParams{Target: linear})
	if done, err := f.s.Disconnect(ctx, server.ID); err != nil || done.State != Disconnected {
		t.Fatalf("mcp = %+v, %v", done, err)
	}
	if want := "lin " + f.project + " false"; len(f.mcp.enabled) != 1 || f.mcp.enabled[0] != want {
		t.Fatalf("enabled = %v", f.mcp.enabled)
	}
}

func TestAServerOnForAFolderAboveIsntReportedOff(t *testing.T) {
	f := newFixture(t)
	f.mcp.servers = []mcp.Server{{ID: "lin", Label: "Linear", URL: "https://mcp.linear.app/mcp", Projects: []string{f.home}}}
	server := f.begin(t, StartParams{Target: linear})
	if _, err := f.s.Disconnect(ctx, server.ID); err == nil {
		t.Fatal("reported off while a folder above keeps it on")
	}
	if got, _ := f.s.Get(server.ID); got.State != Succeeded {
		t.Fatalf("state = %s", got.State)
	}
}

func TestAServerAlsoOnAboveIsntReportedOff(t *testing.T) {
	f := newFixture(t)
	f.mcp.servers = []mcp.Server{{ID: "lin", Label: "Linear", URL: "https://mcp.linear.app/mcp", Projects: []string{f.project, f.home}}}
	server := f.begin(t, StartParams{Target: linear})
	if _, err := f.s.Disconnect(ctx, server.ID); err == nil || len(f.mcp.enabled) != 0 {
		t.Fatalf("reported off while a folder above keeps it on: %v, %v", err, f.mcp.enabled)
	}
}

func TestAnotherAccountProbesAgainBesideTheFirst(t *testing.T) {
	f := newFixture(t)
	f.mcp.auth = mcp.AuthAPIKey
	f.mcp.servers = []mcp.Server{{ID: "lin", Label: "Linear", URL: "https://mcp.linear.app/mcp", Projects: []string{f.project}}}
	if flow := f.begin(t, StartParams{Target: linear}); flow.State != Succeeded {
		t.Fatalf("already connected = %+v", flow)
	}
	again := f.begin(t, StartParams{Target: linear, Account: "again"})
	if again.State != Waiting || f.mcp.another != 0 {
		t.Fatalf("signing in again = %+v", again)
	}
	flow := f.begin(t, StartParams{Target: linear, Account: "another", Label: "Linear (work)"})
	if flow.State != Waiting || flow.Step == nil || f.mcp.another != 1 {
		t.Fatalf("another account = %+v after %d probes", flow, f.mcp.another)
	}

	f.mcp.servers = append(f.mcp.servers, mcp.Server{ID: "lin2", Label: "Linear (work)", URL: "https://mcp.linear.app/mcp",
		Projects: []string{f.project}})
	got := f.s.List(ctx, f.project, []Want{{Target: linear}})
	if got[0].Status != Connected || got[0].Destination != "Linear, Linear (work)" {
		t.Fatalf("list = %+v", got)
	}
	both := f.begin(t, StartParams{Target: linear})
	if _, err := f.s.Disconnect(ctx, both.ID); err == nil {
		t.Fatal("switched off one of two accounts without saying which")
	}
}

func TestALoopbackServerSignsInBeforeAskingForAClient(t *testing.T) {
	f := newFixture(t)
	f.mcp.auth, f.mcp.manual = mcp.AuthOAuth, true
	flow := f.begin(t, StartParams{Target: "mcp:https://mcp.vercel.com", Loopback: true})
	if flow.Step == nil || flow.Step.ID != stepSignIn || !f.mcp.loopback {
		t.Fatalf("start = %+v", flow)
	}
	f.mcp.refuseStart = true
	if flow := f.begin(t, StartParams{Target: "mcp:https://mcp.vercel.com", Loopback: true}); flow.Step == nil || flow.Step.ID != "client_id" {
		t.Fatalf("refused registration = %+v", flow)
	}
}

func TestSigningInAgainPicksTheAccountOnHere(t *testing.T) {
	f := newFixture(t)
	f.mcp.auth = mcp.AuthAPIKey
	f.mcp.servers = []mcp.Server{
		{ID: "lin", Label: "Linear", URL: "https://mcp.linear.app/mcp", Projects: []string{"/other"}},
		{ID: "lin2", Label: "Linear (work)", URL: "https://mcp.linear.app/mcp", Projects: []string{f.project}},
	}
	if flow := f.begin(t, StartParams{Target: linear, Account: "again"}); flow.State != Waiting || len(f.mcp.again) != 1 || f.mcp.again[0] != "lin2" {
		t.Fatalf("again = %+v, %v", flow, f.mcp.again)
	}
	f.mcp.servers[1].Projects = nil
	if _, err := f.s.Start(ctx, StartParams{Project: f.project, Target: linear, Account: "again"}); err == nil {
		t.Fatal("signed in again without knowing which account")
	}
	if _, err := f.s.Start(ctx, StartParams{Project: f.project, Target: linear}); err == nil || len(f.mcp.enabled) != 0 {
		t.Fatalf("switched on one of two accounts: %v, %v", err, f.mcp.enabled)
	}

	// Either account signs in again by its id, on here or not; another
	// server's is refused.
	if flow := f.begin(t, StartParams{Target: linear, Account: "again:lin"}); flow.State != Waiting || flow.Destination != "Linear" ||
		f.mcp.again[len(f.mcp.again)-1] != "lin" {
		t.Fatalf("again:lin = %+v, %v", flow, f.mcp.again)
	}
	if _, err := f.s.Start(ctx, StartParams{Project: f.project, Target: linear, Account: "again:gh"}); errkind.Of(err) != errkind.Invalid {
		t.Fatalf("again:gh = %v", err)
	}
}
