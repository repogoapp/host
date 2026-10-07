package claude

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/claudecode"
)

type turnOutcome struct {
	result agent.Result
	err    error
}
type liveSession struct {
	client                    *claudecode.Client
	catalog                   claudecode.InitializeResponse
	id, key, cwd, fingerprint string
	release                   func()
	mu                        sync.Mutex
	closed                    bool
	io                        *agent.TurnIO
	turnCtx                   context.Context
	turnCancel                context.CancelFunc
	outcome                   chan turnOutcome
	lastActivity              time.Time
	mode                      string
	prePlanMode               string
	allowBypass               bool
	tasks                     map[string]*liveTask
	blocks                    map[string]map[int]*streamBlock
	messageID                 string
	calls                     map[string]string
	seenResults               map[string]bool
	seenMessages              map[string]bool
	deferred                  *turnOutcome
	usage                     agent.Usage
	lastModel                 string
	promptID                  string
	planRejected              string
	planRejectionSeen         bool
	promptSent                bool
	promptEchoed              bool
	sawText                   bool
	owedIdle                  int
	// working is Claude's own state: true from "running" to "idle", including
	// a turn it starts by itself when a background task finishes.
	working bool
	// The running turn is one Claude started by itself (join), not a prompt.
	joined bool
	// ownTurn tells the Manager Claude started a turn by itself.
	ownTurn  func()
	pending  int
	consumed chan struct{}
}

func newLiveSession(id, cwd string) *liveSession {
	return &liveSession{id: id, cwd: cwd, lastActivity: time.Now(), tasks: map[string]*liveTask{}, consumed: make(chan struct{}), mode: "default"}
}

// send runs one turn: the prompt goes to Claude, and the turn ends with its
// result, the process exiting, or a stop.
func (s *liveSession) send(ctx context.Context, req agent.TurnRequest, io agent.TurnIO) (agent.Result, error) {
	s.mu.Lock()
	if s.closed || s.io != nil {
		s.mu.Unlock()
		return agent.Result{}, errors.New("Claude session closed")
	}
	turnCtx, outcome, finish := s.beginLocked(ctx, io)
	s.mu.Unlock()
	defer finish()
	if io.Session != nil {
		io.Session(s.id)
	}
	configCtx, configCancel := context.WithTimeout(turnCtx, 20*time.Second)
	err := s.applyConfig(configCtx, req.Config)
	configCancel()
	if err != nil {
		return agent.Result{SessionID: s.id}, err
	}
	message := userMessage(req, s.id)
	s.mu.Lock()
	s.promptID = message.UUID
	s.promptSent = true
	s.mu.Unlock()
	if err = s.client.Send(turnCtx, message); err != nil {
		return agent.Result{SessionID: s.id}, err
	}
	return s.wait(ctx, outcome)
}

// join runs the turn Claude started by itself as the host's: it ends with
// Claude's result or idle, the process exiting, or a stop.
func (s *liveSession) join(ctx context.Context, io agent.TurnIO) (agent.Result, error) {
	s.mu.Lock()
	if s.closed || s.io != nil || !s.working {
		s.mu.Unlock()
		return agent.Result{SessionID: s.id}, nil
	}
	_, outcome, finish := s.beginLocked(ctx, io)
	s.joined = true
	s.promptSent = true
	s.mu.Unlock()
	defer finish()
	if io.Session != nil {
		io.Session(s.id)
	}
	return s.wait(ctx, outcome)
}

// beginLocked binds io as the session's running turn; finish unbinds it.
func (s *liveSession) beginLocked(ctx context.Context, io agent.TurnIO) (context.Context, <-chan turnOutcome, func()) {
	turnCtx, cancel := context.WithCancel(ctx)
	s.io = &io
	s.turnCtx = turnCtx
	s.turnCancel = cancel
	s.outcome = make(chan turnOutcome, 1)
	s.blocks = map[string]map[int]*streamBlock{}
	s.messageID = ""
	s.calls = map[string]string{}
	s.seenResults = map[string]bool{}
	s.seenMessages = map[string]bool{}
	s.deferred = nil
	s.usage = agent.Usage{}
	s.planRejected = ""
	for id, task := range s.tasks {
		if task.terminal {
			delete(s.tasks, id)
		}
	}
	s.planRejectionSeen = false
	s.promptSent = false
	s.promptEchoed = false
	s.sawText = false
	s.joined = false
	finish := func() {
		cancel()
		s.mu.Lock()
		s.io = nil
		s.turnCtx = nil
		s.turnCancel = nil
		s.outcome = nil
		s.joined = false
		s.lastActivity = time.Now()
		s.mu.Unlock()
	}
	return turnCtx, s.outcome, finish
}

func (s *liveSession) wait(ctx context.Context, outcome <-chan turnOutcome) (agent.Result, error) {
	select {
	case result := <-outcome:
		return result.result, result.err
	case <-s.consumed:
		return agent.Result{SessionID: s.id}, processError(s.client.Exit())
	case <-ctx.Done():
	}
	return s.stop(ctx, outcome)
}

// stop interrupts the turn once its context ends, and waits for Claude to
// unwind it; past the grace period the process is closed.
func (s *liveSession) stop(ctx context.Context, outcome <-chan turnOutcome) (agent.Result, error) {
	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stopCancel()
	go func() { _ = s.client.Interrupt(stopCtx) }()
	select {
	case <-outcome:
	case <-s.consumed:
	case <-stopCtx.Done():
		s.close()
	}
	return agent.Result{SessionID: s.id, StopReason: "cancelled"}, ctx.Err()
}

func (s *liveSession) consume() {
	defer close(s.consumed)
	for message := range s.client.Messages() {
		s.handleMessage(message)
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
	if s.io != nil || s.pending > 0 {
		return true
	}
	for _, task := range s.tasks {
		if !task.terminal && !task.ambient {
			return true
		}
	}
	return false
}
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
func (s *liveSession) Running() (agent.RunningTurn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.io == nil || s.closed {
		return agent.RunningTurn{}, false
	}
	return agent.RunningTurn{Ctx: s.turnCtx, TurnID: s.io.TurnID, ChatID: s.key, Cwd: s.cwd, Device: s.io.Device}, true
}
func (s *liveSession) ask(ctx context.Context, approval agent.Approval) (json.RawMessage, error) {
	s.mu.Lock()
	io, turnCtx := s.io, s.turnCtx
	if io == nil || s.closed {
		s.mu.Unlock()
		return nil, errors.New("no Claude turn is running")
	}
	s.pending++
	s.mu.Unlock()
	requestCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(turnCtx, cancel)
	defer func() { stop(); cancel(); s.mu.Lock(); s.pending--; s.mu.Unlock() }()
	return io.Ask(requestCtx, approval)
}
