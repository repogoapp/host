package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/codexappserver"
)

// How long an interrupted turn may unwind before the process is closed; past
// this, a turn still running would interleave with the next one.
const stopGrace = 5 * time.Second

type turnOutcome struct {
	result agent.Result
	err    error
}

// liveSession is one `codex app-server` holding one thread, alive across the
// chat's turns. A turn binds itself for as long as it runs and receives
// everything the thread emits meanwhile; anything outside a turn is dropped.
type liveSession struct {
	client                    *codexappserver.Client
	log                       *slog.Logger
	id, key, cwd, fingerprint string
	release                   func()
	consumed                  chan struct{}

	mu     sync.Mutex
	closed bool
	// What the thread runs with, which plan mode must repeat.
	model, effort string
	pending       int
	lastActivity  time.Time

	io         *agent.TurnIO
	turnCtx    context.Context
	turnCancel context.CancelFunc
	outcome    chan turnOutcome
	// Codex's id for the bound turn; empty until turn/start answers, when
	// named closes. Codex can stream before that answer is read.
	turnID string
	named  chan struct{}
	// Turns that completed before turn/start answered with their id.
	finished map[string]codexappserver.Turn
	failure  string
	usage    agent.Usage
	names    map[string]string
	own      map[string]bool
	streamed map[string]bool
	changes  map[string]json.RawMessage
}

func newLiveSession(cwd string, log *slog.Logger) *liveSession {
	return &liveSession{cwd: cwd, log: log, consumed: make(chan struct{}), lastActivity: time.Now()}
}

// send runs one turn: turn/start, then the turn ends with turn/completed,
// the process exiting, or a stop.
func (s *liveSession) send(ctx context.Context, req agent.TurnRequest, io agent.TurnIO) (agent.Result, error) {
	s.mu.Lock()
	if s.closed || s.io != nil {
		s.mu.Unlock()
		return agent.Result{}, errors.New("Codex session closed")
	}
	turnCtx, cancel := context.WithCancel(ctx)
	s.io, s.turnCtx, s.turnCancel = &io, turnCtx, cancel
	s.outcome = make(chan turnOutcome, 1)
	s.turnID, s.failure = "", ""
	s.named = make(chan struct{})
	s.finished = map[string]codexappserver.Turn{}
	s.usage = agent.Usage{}
	s.names, s.own, s.streamed = map[string]string{}, map[string]bool{}, map[string]bool{}
	s.changes = map[string]json.RawMessage{}
	outcome := s.outcome
	params := s.turnParams(req)
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.io, s.turnCtx, s.turnCancel, s.outcome = nil, nil, nil, nil
		s.lastActivity = time.Now()
		s.mu.Unlock()
	}()
	if io.Session != nil {
		io.Session(s.id)
	}
	turn, err := s.client.TurnStart(turnCtx, params)
	if err != nil {
		return agent.Result{SessionID: s.id}, fmt.Errorf("start Codex turn: %w", withStderr(err, s.client))
	}
	s.mu.Lock()
	s.turnID = turn.ID
	close(s.named)
	if done, ok := s.finished[turn.ID]; ok {
		s.completeLocked(done)
	}
	s.mu.Unlock()
	select {
	case result := <-outcome:
		return result.result, result.err
	case <-s.consumed:
		return agent.Result{SessionID: s.id}, processError(s.client.Exit())
	case <-ctx.Done():
	}
	return s.stop(ctx, turn.ID, outcome)
}

// steer adds req to the bound turn when it is turnID, waiting for Codex to
// name it. It never retries another id: a mismatch means the turn has ended.
func (s *liveSession) steer(ctx context.Context, turnID string, req agent.TurnRequest) error {
	s.mu.Lock()
	io, turnCtx, named := s.io, s.turnCtx, s.named
	s.mu.Unlock()
	if io == nil || io.TurnID != turnID {
		return errors.New("no Codex turn to steer")
	}
	select {
	case <-named:
	case <-turnCtx.Done():
		return errors.New("no Codex turn to steer")
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	bound, codexTurn := s.io == io, s.turnID
	s.mu.Unlock()
	if !bound {
		return errors.New("no Codex turn to steer")
	}
	_, err := s.client.TurnSteer(ctx, codexappserver.SteerParams{
		ThreadID: s.id, ExpectedTurnID: codexTurn, Input: prompt(req), ClientUserMessageID: uuid.NewString(),
	})
	return err
}

// stop interrupts the turn once its context ends, and waits for Codex to
// complete it as interrupted; past the grace period the process is closed.
func (s *liveSession) stop(ctx context.Context, turnID string, outcome <-chan turnOutcome) (agent.Result, error) {
	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), stopGrace)
	defer stopCancel()
	go func() { _ = s.client.TurnInterrupt(stopCtx, s.id, turnID) }()
	select {
	case <-outcome:
	case <-s.consumed:
	case <-stopCtx.Done():
		s.close()
	}
	return agent.Result{SessionID: s.id, StopReason: "cancelled"}, ctx.Err()
}

// turnParams maps the turn's settings onto turn/start. They stick for the
// thread's later turns, so plan mode is set or cleared on every turn.
func (s *liveSession) turnParams(req agent.TurnRequest) codexappserver.TurnParams {
	cfg := req.Config
	p := codexappserver.TurnParams{ThreadID: s.id, Input: prompt(req), Model: cfg.Model, Effort: cfg.ReasoningLevel}
	switch cfg.PermissionMode {
	case agent.PermissionApprovalRequired:
		p.ApprovalPolicy, p.SandboxPolicy = "on-request", &codexappserver.SandboxPolicy{Type: "readOnly"}
	case agent.PermissionAutoAcceptEdits:
		p.ApprovalPolicy, p.SandboxPolicy = "on-request", &codexappserver.SandboxPolicy{Type: "workspaceWrite"}
	case agent.PermissionFullAccess:
		p.ApprovalPolicy, p.SandboxPolicy = "never", &codexappserver.SandboxPolicy{Type: "dangerFullAccess"}
	}
	p.ServiceTierForTurn = "default"
	if cfg.FastMode {
		p.ServiceTierForTurn = "fast"
	}
	if cfg.Model != "" {
		s.model = cfg.Model
	}
	if cfg.ReasoningLevel != "" {
		s.effort = cfg.ReasoningLevel
	}
	// The mode needs a model to run; without one Codex keeps its own.
	if s.model != "" {
		mode := &codexappserver.CollaborationMode{Mode: "default"}
		if cfg.Mode == "plan" {
			mode.Mode = "plan"
		}
		mode.Settings.Model = s.model
		if s.effort != "" {
			effort := s.effort
			mode.Settings.ReasoningEffort = &effort
		}
		p.CollaborationMode = mode
	}
	return p
}

// prompt is the turn's input: its text, each image by path so Codex sees
// the pixels, and every other file as a mention it can open.
func prompt(req agent.TurnRequest) []codexappserver.Input {
	var input []codexappserver.Input
	if req.Prompt != "" {
		input = append(input, codexappserver.Input{Type: "text", Text: req.Prompt})
	}
	for _, a := range req.Attachments {
		if a.IsImage() {
			input = append(input, codexappserver.Input{Type: "localImage", Path: a.Path})
			continue
		}
		input = append(input, codexappserver.Input{Type: "mention", Name: a.Name, Path: a.Path})
	}
	return input
}

// completeLocked settles the bound turn from Codex's final word on it.
func (s *liveSession) completeLocked(turn codexappserver.Turn) {
	if s.outcome == nil {
		return
	}
	usage := s.usage
	result := agent.Result{SessionID: s.id, StopReason: turn.Status, Usage: &usage}
	var err error
	switch turn.Status {
	case "interrupted":
		result.StopReason = "cancelled"
	case "failed":
		message := s.failure
		if turn.Error != nil && turn.Error.Message != "" {
			message = turn.Error.Message
		}
		if message == "" {
			message = "Codex turn failed"
		}
		err = errors.New(message)
	}
	select {
	case s.outcome <- turnOutcome{result: result, err: err}:
	default:
	}
}

func processError(exit codexappserver.Exit) error {
	if exit.Err != nil {
		return fmt.Errorf("Codex exited (%d): %w", exit.Code, exit.Err)
	}
	return fmt.Errorf("Codex exited (%d)", exit.Code)
}

// consume reads the thread's notifications until the process exits, which
// ends the bound turn.
func (s *liveSession) consume() {
	defer close(s.consumed)
	for n := range s.client.Notifications() {
		s.handle(n)
	}
	<-s.client.Done()
	s.mu.Lock()
	if s.turnCancel != nil {
		s.turnCancel()
	}
	s.mu.Unlock()
}

func (s *liveSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.turnCancel != nil {
		s.turnCancel()
	}
	s.mu.Unlock()
	_ = s.client.Close()
	if s.release != nil {
		s.release()
	}
}

func (s *liveSession) isClosed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closed }
func (s *liveSession) busyLocked() bool {
	return s.io != nil || s.pending > 0
}

// reapable reports a session that is finished with: idle past the cutoff, or
// a process that exited while nobody was using it.
func (s *liveSession) reapable(cutoff time.Time) bool {
	select {
	case <-s.client.Done():
		return true
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.busyLocked() && s.lastActivity.Before(cutoff)
}

// Running is the turn bound to this process, for RepoGo's tools.
func (s *liveSession) Running() (agent.RunningTurn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.io == nil || s.closed {
		return agent.RunningTurn{}, false
	}
	return agent.RunningTurn{Ctx: s.turnCtx, TurnID: s.io.TurnID, ChatID: s.key, Cwd: s.cwd, Device: s.io.Device}, true
}

// ask blocks on a person for the bound turn. Codex resolving the request, or
// the turn ending, withdraws it.
func (s *liveSession) ask(ctx context.Context, approval agent.Approval) (json.RawMessage, error) {
	s.mu.Lock()
	io, turnCtx := s.io, s.turnCtx
	if io == nil || s.closed {
		s.mu.Unlock()
		return nil, errors.New("no Codex turn is running")
	}
	s.pending++
	s.mu.Unlock()
	requestCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(turnCtx, cancel)
	defer func() { stop(); cancel(); s.mu.Lock(); s.pending--; s.mu.Unlock() }()
	return io.Ask(requestCtx, approval)
}

// emit hands an event to the bound turn; with none bound it is dropped.
func (s *liveSession) emit(e agent.Event) {
	s.mu.Lock()
	io := s.io
	s.mu.Unlock()
	if io != nil {
		io.Emit(e)
	}
}
