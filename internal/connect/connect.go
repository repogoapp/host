// Package connect lets the user connect something from the phone (a key in a
// dotenv file, an MCP server, a CLI's sign-in) and tells cards and agents
// whether it is connected, never with what. Each kind keeps its own storage;
// a flow is one attempt: started, answered step by step, then ended.
package connect

import (
	"context"
	"crypto/rand"
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
	OAuthFinish(ctx context.Context, probeID, code, state string) (mcp.Server, error)
	SetEnabled(id, project string, enabled bool) error
}

// CLIs is the CLIs on this machine and their own sign-ins.
type CLIs interface {
	Get(ctx context.Context, kind string) (clitool.Install, bool)
	LoginStart(ctx context.Context, kind string) (clilogin.Code, error)
	LoginComplete(ctx context.Context, kind, code string) error
	LoginCancel(kind string)
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
	Scope  string `json:"scope,omitempty" doc:"For an env key, its file: plugin:<name>, plugins, source:<handle> or project:<.env file>"`
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
			c.Status, c.Reason = Unavailable, err.Error()
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
		err = s.startEnv(f, p.Note, help)
	case KindMCP:
		err = s.startMCP(ctx, f, p.Label, p.Note, help)
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
	s.mu.Lock()
	expired := s.sweepLocked()
	f, err := waitingLocked(s.flows, flowID, stepID)
	var snapshot flow
	if err == nil {
		snapshot = *f
	}
	s.mu.Unlock()
	s.ended(expired)
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
		// The CLI's own exit says how the sign-in went (CLIEnded).
		if err := s.d.CLIs.LoginComplete(ctx, snapshot.target.CLI, value); err != nil {
			return s.advance(f, stepID, nil, err), nil
		}
		return s.Get(flowID)
	}
	if errkind.Of(err) == errkind.Invalid {
		return Flow{}, err
	}
	return s.advance(f, stepID, next, err), nil
}

// Callback hands an MCP server's OAuth redirect to the flow its state names.
// The first callback claims the flow; a second has nothing to exchange.
func (s *Service) Callback(ctx context.Context, state, code, providerErr string) (Flow, error) {
	s.mu.Lock()
	expired := s.sweepLocked()
	var f *flow
	for _, candidate := range s.flows {
		if state != "" && candidate.state == state {
			f = candidate
		}
	}
	var err error
	switch {
	case f == nil:
		err = ErrNotFound.Errorf("no sign-in is waiting for this redirect")
	case f.State != Waiting || f.claimed:
		err = ErrInvalid.Errorf("this sign-in has already been used")
	default:
		f.claimed = true
	}
	var stepID, probeID string
	if err == nil {
		stepID, probeID = f.Step.ID, f.probeID
	}
	s.mu.Unlock()
	s.ended(expired)
	if err != nil {
		return Flow{}, err
	}
	if providerErr != "" {
		return s.advance(f, stepID, nil, ErrInvalid.Errorf("%s", providerErr)), nil
	}
	_, err = s.d.MCP.OAuthFinish(ctx, probeID, code, state)
	return s.advance(f, stepID, nil, err), nil
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
	if ok && f.State == Waiting {
		f.end(Cancelled, "", s.d.Now())
		stopped = append(stopped, f)
	}
	s.mu.Unlock()
	s.ended(stopped)
	if !ok {
		return ErrNotFound
	}
	return nil
}

// advance moves a flow on from stepID: to next, or to its end with err as its
// reason. A flow that ended meanwhile (cancelled, expired) keeps that ending.
func (s *Service) advance(f *flow, stepID string, next *Step, err error) Flow {
	s.mu.Lock()
	moved := f.State == Waiting && f.Step != nil && f.Step.ID == stepID
	switch {
	case !moved:
	case err != nil:
		f.end(Failed, err.Error(), s.d.Now())
	case next != nil:
		f.Step, f.claimed = next, false
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
		if f.target.Kind == KindCLI {
			s.d.CLIs.LoginCancel(f.target.CLI)
		}
		s.changed(f.Flow)
	}
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
	}
	return f, nil
}

func (s *Service) changed(f Flow) {
	s.d.Changed(Changed{Flow: f})
}
