// Package connect lets the user connect something from the phone (a key in a
// dotenv file, an MCP server, a CLI's sign-in) and tells cards and agents
// whether it is connected, never with what. Each kind keeps its own storage;
// a flow is one attempt: started, answered step by step, then ended.
package connect

import (
	"context"
	"crypto/rand"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/clilogin"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/envsource"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/mcp"
)

var (
	ErrInvalid  = errkind.New(errkind.Invalid, "connect: invalid")
	ErrNotFound = errkind.New(errkind.NotFound, "connect: no such flow")
)

// waitFor is how long a flow waits for the user; kept is how long a finished
// one stays readable, so a phone back from the browser still learns how it ended.
const (
	waitFor = 10 * time.Minute
	kept    = 5 * time.Minute
)

// MCPs is the MCP servers a flow connects: the same probe and sign-in
// RepoGo's own MCP screen uses.
type MCPs interface {
	List(project string) mcp.Listing
	Probe(ctx context.Context, rawURL, label, project string) (mcp.Probe, error)
	Connect(probeID, apiKey string) (mcp.Server, error)
	OAuthStart(ctx context.Context, probeID, clientID string, loopback bool) (authorizeURL, state string, err error)
	ProbeAnother(ctx context.Context, rawURL, label, project string) (mcp.Probe, error)
	ProbeAgain(ctx context.Context, id, project string) (mcp.Probe, error)
	OAuthFinish(ctx context.Context, probeID, code, state string) (mcp.Server, error)
	SetEnabled(id, project string, enabled bool) error
}

// CLIs is the CLIs on this machine and their own sign-ins.
type CLIs interface {
	Get(ctx context.Context, kind string) (clitool.Install, bool)
	LoginStart(ctx context.Context, kind string) (clilogin.Code, error)
	LoginComplete(ctx context.Context, kind, code string) error
	LoginCancel(kind string)
	Logout(ctx context.Context, kind string) (clitool.UpdateResult, error)
}

type Deps struct {
	// Sources is the env sources a `source:` scope names.
	Sources *envsource.Sources
	// PluginDir is the folder of a plugin a project can use, the project's own first.
	PluginDir func(project, plugin string) (string, error)
	// SharedEnv is the `.env` every plugin on this environment reads.
	SharedEnv string
	// Contain proves a path lives inside a known project.
	Contain func(path string) (string, error)
	// Home is the folder a `file:` path must stay under.
	Home string
	// Ignored is git's answer to which paths it ignores, for the warning
	// that a key could be committed.
	Ignored func(ctx context.Context, dir string, paths []string) (map[string]bool, error)
	MCP     MCPs
	CLIs    CLIs
	// Changed is told each flow as it starts, moves and ends.
	Changed func(Changed)
	Now     func() time.Time
}

type Service struct {
	d Deps
	// writeMu makes writing a dotenv file one flow's job at a time.
	writeMu sync.Mutex

	mu    sync.Mutex
	flows map[string]*flow
}

func New(d Deps) *Service {
	return &Service{d: d, flows: map[string]*flow{}}
}

func init() {
	emit.Register(Changed{})
}

// Changed is a flow as it starts, moves and ends, sent to every paired device,
// so a card showing whether something is connected reads it again.
type Changed struct {
	Flow Flow `json:"flow"`
}

func (Changed) Method() string { return "connect.changed" }

// Want is a target a caller asks about.
type Want struct {
	Target string `json:"target" doc:"What is connected: env:<NAME>, mcp:<url> or cli:<tool>"`
	Scope  string `json:"scope,omitempty" doc:"For an env key, its file: file:<path>, plugin:<name>, plugins or source:<handle>"`
}

// Connection is a target's state: whether it is connected, never with what.
type Connection struct {
	Target string `json:"target"`
	Scope  string `json:"scope"`
	// Status is configured (an env key has a value), connected, missing, or
	// unavailable when it can't be connected here.
	Status string `json:"status"`
	// Destination is where it lives, as the host words it.
	Destination string `json:"destination"`
	// Account is who a CLI is signed in as, when it says.
	Account string `json:"account,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

const (
	Configured  = "configured"
	Connected   = "connected"
	Missing     = "missing"
	Unavailable = "unavailable"
)

// List is each wanted target's state, in the order asked.
func (s *Service) List(ctx context.Context, project string, wants []Want) []Connection {
	out := make([]Connection, 0, len(wants))
	for _, w := range wants {
		c := Connection{Target: w.Target, Scope: w.Scope}
		t, err := parseTarget(w.Target, w.Scope)
		if err == nil {
			switch t.Kind {
			case KindEnv:
				err = s.envStatus(project, t.Env, &c)
			case KindMCP:
				s.mcpStatus(project, t.URL, &c)
			case KindCLI:
				err = s.cliStatus(ctx, t.CLI, &c)
			}
		}
		if err != nil {
			c.Status, c.Reason = Unavailable, reason(err)
		}
		out = append(out, c)
	}
	return out
}

// Start opens a flow for a target. Its steps, and where it lands, are the
// host's to word whatever the caller wrote; one with nothing to ask of the
// user ends at once.
func (s *Service) Start(ctx context.Context, p StartParams) (Flow, error) {
	t, err := parseTarget(p.Target, p.Scope)
	if err != nil {
		return Flow{}, err
	}
	help, err := helpURL(p.HelpURL)
	if err != nil {
		return Flow{}, err
	}
	if len(p.Note) > maxNote {
		return Flow{}, ErrInvalid.Errorf("a note is at most %d characters", maxNote)
	}
	f := &flow{
		Flow:    Flow{ID: rand.Text(), Target: p.Target, Scope: p.Scope, State: Waiting},
		project: p.Project, target: t, started: s.d.Now(), loopback: p.Loopback,
	}
	switch t.Kind {
	case KindEnv:
		err = s.startEnv(ctx, f, p.Note, help)
	case KindMCP:
		err = s.startMCP(ctx, f, p.Label, p.Note, help, p.Account)
	case KindCLI:
		err = s.startCLI(ctx, f)
	}
	if err != nil {
		return Flow{}, err
	}
	s.mu.Lock()
	expired := s.sweepLocked()
	s.flows[f.ID] = f
	out := f.Flow
	s.mu.Unlock()
	s.ended(expired)
	s.changed(out)
	return out, nil
}

// Submit answers a flow's waiting step. A value the target refuses leaves the
// flow waiting for another; one that fails otherwise ends it.
func (s *Service) Submit(ctx context.Context, flowID, stepID, value string) (Flow, error) {
	f, snapshot, err := s.claim(flowID, stepID)
	if err != nil {
		return Flow{}, err
	}

	var next *Step
	switch snapshot.target.Kind {
	case KindEnv:
		err = s.submitEnv(snapshot, value)
	case KindMCP:
		next, err = s.submitMCP(ctx, snapshot, value)
	case KindCLI:
		if strings.TrimSpace(value) == "" {
			err = ErrInvalid.Errorf("paste the code the page shows")
			break
		}
		// The CLI's own exit says how the sign-in went (CLIEnded); its output
		// is never the reason, as it can echo what was typed.
		if s.d.CLIs.LoginComplete(ctx, snapshot.target.CLI, value) != nil {
			err = errSignIn
		}
		return s.advance(f, stepID, nil, err), nil
	}
	if errkind.Of(err) == errkind.Invalid {
		s.release(f)
		return Flow{}, err
	}
	return s.advance(f, stepID, next, err), nil
}

// claim takes a waiting step for one submit or callback, under the lock, and
// returns a copy to act on outside it. A second claim waits for advance.
func (s *Service) claim(flowID, stepID string) (*flow, flow, error) {
	s.mu.Lock()
	expired := s.sweepLocked()
	f, err := waitingLocked(s.flows, flowID, stepID)
	var snapshot flow
	if err == nil {
		f.working = true
		snapshot = *f
	}
	s.mu.Unlock()
	s.ended(expired)
	return f, snapshot, err
}

// release hands a claimed step back, still waiting, after a value it refused.
func (s *Service) release(f *flow) {
	s.mu.Lock()
	f.working = false
	s.mu.Unlock()
}

// Callback hands an MCP server's OAuth redirect to its flow, which must be
// waiting on its sign-in with the state the redirect carries. The first
// callback claims it; a second finds it taken.
func (s *Service) Callback(ctx context.Context, flowID, state, code, providerErr string) (Flow, error) {
	f, snapshot, err := s.claim(flowID, stepSignIn)
	if err != nil {
		return Flow{}, err
	}
	if state == "" || state != snapshot.state {
		s.release(f)
		return Flow{}, ErrInvalid.Errorf("this redirect is from another sign-in")
	}
	switch {
	case providerErr != "":
		// The provider's words can carry anything; its code alone says why.
		err = ErrInvalid.Errorf("The sign-in was declined or failed (%s)", safeCode(providerErr))
	default:
		_, err = s.d.MCP.OAuthFinish(ctx, snapshot.probeID, code, state)
	}
	return s.advance(f, stepSignIn, nil, err), nil
}

// Get is a flow as it stands, for a phone that missed how it ended.
func (s *Service) Get(flowID string) (Flow, error) {
	s.mu.Lock()
	expired := s.sweepLocked()
	f, ok := s.flows[flowID]
	var out Flow
	if ok {
		out = f.Flow
	}
	s.mu.Unlock()
	s.ended(expired)
	if !ok {
		return Flow{}, ErrNotFound
	}
	return out, nil
}

// Cancel ends a waiting flow; one already ended keeps its ending.
func (s *Service) Cancel(flowID string) error {
	s.mu.Lock()
	stopped := s.sweepLocked()
	f, ok := s.flows[flowID]
	working := ok && f.working
	if ok && f.State == Waiting && !working {
		f.end(Cancelled, "", s.d.Now())
		stopped = append(stopped, f)
	}
	s.mu.Unlock()
	s.ended(stopped)
	switch {
	case !ok:
		return ErrNotFound
	case working:
		return ErrInvalid.Errorf("it's saving; wait for it to finish")
	}
	return nil
}

// Disconnect undoes a connected flow: signs its CLI out on this environment,
// switches its MCP server off for the project, or removes its env key. A
// second call answers with the same ending.
func (s *Service) Disconnect(ctx context.Context, flowID string) (Flow, error) {
	s.mu.Lock()
	f, ok := s.flows[flowID]
	var err error
	switch {
	case !ok:
		err = ErrNotFound
	case f.State == Disconnected:
		out := f.Flow
		s.mu.Unlock()
		return out, nil
	case f.State != Succeeded:
		err = ErrInvalid.Errorf("only a connected flow can be disconnected")
	case f.working:
		err = ErrInvalid.Errorf("it's already disconnecting")
	default:
		f.working = true
	}
	var snapshot flow
	if err == nil {
		snapshot = *f
	}
	s.mu.Unlock()
	if err != nil {
		return Flow{}, err
	}

	switch snapshot.target.Kind {
	case KindEnv:
		err = s.disconnectEnv(snapshot)
	case KindMCP:
		err = s.disconnectMCP(snapshot)
	case KindCLI:
		err = s.disconnectCLI(ctx, snapshot)
	}

	s.mu.Lock()
	f.working = false
	if err == nil {
		f.State, f.ended = Disconnected, s.d.Now()
	}
	out := f.Flow
	s.mu.Unlock()
	if err != nil {
		return Flow{}, err
	}
	s.changed(out)
	return out, nil
}

// Tick expires flows nobody finished, on the host's minute ticker, so a
// sign-in left open stops its CLI even when no call comes in.
func (s *Service) Tick(_ context.Context, _ time.Time) {
	s.mu.Lock()
	expired := s.sweepLocked()
	s.mu.Unlock()
	s.ended(expired)
}

// advance moves a flow on from stepID: to next, or to its end with err as its
// reason. A flow that ended meanwhile (cancelled, expired) keeps that ending.
func (s *Service) advance(f *flow, stepID string, next *Step, err error) Flow {
	s.mu.Lock()
	moved := f.State == Waiting && f.Step != nil && f.Step.ID == stepID
	f.working = false
	switch {
	case !moved:
	case err != nil:
		f.end(Failed, reason(err), s.d.Now())
	case next != nil:
		f.Step = next
	default:
		f.end(Succeeded, "", s.d.Now())
	}
	out := f.Flow
	s.mu.Unlock()
	if moved {
		s.changed(out)
	}
	return out
}

// sweepLocked expires flows nobody finished, returning them to wind down once
// unlocked, and forgets finished ones.
func (s *Service) sweepLocked() []*flow {
	now := s.d.Now()
	var expired []*flow
	for id, f := range s.flows {
		// A flow mid-save or mid-disconnect keeps going; it ends with its own answer.
		if f.working {
			continue
		}
		switch {
		case f.State == Waiting && now.Sub(f.started) >= waitFor:
			f.end(Expired, "", now)
			expired = append(expired, f)
		case f.State != Waiting && now.Sub(f.ended) >= kept:
			delete(s.flows, id)
		}
	}
	return expired
}

// ended announces flows that stopped waiting without a step answered, and
// stops the CLI sign-in nobody will finish.
func (s *Service) ended(flows []*flow) {
	for _, f := range flows {
		if f.target.Kind == KindCLI && !s.loginShared(f.target.CLI) {
			s.d.CLIs.LoginCancel(f.target.CLI)
		}
		s.changed(f.Flow)
	}
}

// loginShared is whether another flow still waits on a CLI's sign-in, which
// the CLI runs once for all of them.
func (s *Service) loginShared(kind string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.flows {
		if f.target.Kind == KindCLI && f.target.CLI == kind && f.State == Waiting {
			return true
		}
	}
	return false
}

// waitingLocked is the flow whose step stepID is waiting for the user.
func waitingLocked(flows map[string]*flow, flowID, stepID string) (*flow, error) {
	f, ok := flows[flowID]
	switch {
	case !ok:
		return nil, ErrNotFound
	case f.State != Waiting:
		return nil, ErrInvalid.Errorf("this flow has %s", f.State)
	case f.Step == nil || f.Step.ID != stepID:
		return nil, ErrInvalid.Errorf("no step %q is waiting", stepID)
	case f.working:
		return nil, ErrInvalid.Errorf("it's already saving")
	}
	return f, nil
}

// errSignIn is a CLI sign-in that ended without signing in, worded here
// because the CLI's own output can echo what was typed.
var errSignIn = errkind.New(errkind.Unavailable, "The sign-in didn't finish. Try again.")

// safeCode keeps an OAuth error code (access_denied) and nothing else.
func safeCode(code string) string {
	if oauthCode.MatchString(code) {
		return code
	}
	return "unknown"
}

var oauthCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// reason is err as the phone shows it: the detail, without its kind's words.
func reason(err error) string {
	var kinded *errkind.Error
	if errors.Is(err, errSignIn) {
		return errSignIn.Error()
	}
	if errors.As(err, &kinded) {
		return strings.TrimPrefix(err.Error(), kinded.Error()+": ")
	}
	return err.Error()
}

func (s *Service) changed(f Flow) {
	s.d.Changed(Changed{Flow: f})
}
