package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/cursoragent"
)

type liveSession struct {
	provider     *Provider
	client       *cursoragent.Client
	id, key, cwd string
	fingerprint  [32]byte
	state        cursoragent.Session
	images       bool
	release      func()
	mu           sync.Mutex
	closed       bool
	lastActivity time.Time
	io           *agent.TurnIO
	turnCtx      context.Context
	turnCancel   context.CancelFunc
	permission   agent.PermissionMode
	tools        map[string]cursoragent.ToolCall
	denied       map[string]bool
	finished     map[string]bool
	history      []agent.Event
	replaying    bool
	historyTurn  string
	historyTurns int
	nativeSize   int64
	nativeTime   time.Time
}

func (s *liveSession) send(ctx context.Context, req agent.TurnRequest, stream agent.TurnIO) (agent.Result, error) {
	result := agent.Result{SessionID: s.id}
	content, err := prompt(req, s.images)
	if err != nil {
		return result, err
	}
	configure, cancelConfigure := context.WithTimeout(ctx, 30*time.Second)
	defer cancelConfigure()
	if req.Config.Model != "" {
		if err = s.client.SetModel(configure, s.id, req.Config.Model); err != nil {
			return result, err
		}
		s.mu.Lock()
		s.state.Models.CurrentModelID = req.Config.Model
		s.mu.Unlock()
	}
	mode := req.Config.Mode
	if mode == "" {
		mode = "agent"
	}
	if err = s.client.SetMode(configure, s.id, mode); err != nil {
		return result, err
	}
	req.Config.Mode = mode
	s.mu.Lock()
	s.state.Modes.CurrentModeID = mode
	s.mu.Unlock()

	turnCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.closed || s.io != nil {
		s.mu.Unlock()
		cancel()
		return result, errors.New("Cursor session unavailable")
	}
	s.io, s.turnCtx, s.turnCancel = &stream, turnCtx, cancel
	s.permission = req.Config.PermissionMode
	s.resetToolsLocked()
	s.historyTurns++
	s.historyTurn = fmt.Sprintf("%s:%d", s.id, s.historyTurns)
	turn := s.historyTurn
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.io = nil
		s.turnCtx = nil
		s.turnCancel = nil
		s.lastActivity = time.Now()
		s.mu.Unlock()
	}()
	started := time.Now().UnixMilli()
	if err = s.record(agent.Event{Kind: agent.EventTurnStarted, At: started}); err != nil {
		return result, err
	}
	user := req.Prompt
	for _, a := range req.Attachments {
		user += "\n[@" + a.Name + "](" + a.Path + ")"
	}
	if err = s.record(agent.Event{Kind: agent.EventUserMessage, Text: user, At: started}); err != nil {
		return result, err
	}
	if stream.Session != nil {
		stream.Session(s.id)
	}
	type outcome struct {
		result cursoragent.PromptResult
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		response, err := s.client.Prompt(context.WithoutCancel(turnCtx), s.id, content)
		finished <- outcome{response, err}
	}()
	select {
	case done := <-finished:
		result.StopReason, err = done.result.StopReason, done.err
	case <-ctx.Done():
		stop, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = s.client.Cancel(stop, s.id)
		select {
		case <-finished:
		case <-stop.Done():
			s.close()
		}
		stopCancel()
		result.StopReason, err = "cancelled", ctx.Err()
	}
	if ctx.Err() != nil {
		result.StopReason, err = "cancelled", ctx.Err()
	}
	if result.StopReason == "cancelled" && err == nil {
		err = context.Canceled
	}
	final := agent.Event{Kind: agent.EventTurnFinished, At: time.Now().UnixMilli()}
	if err != nil {
		final.Kind = agent.EventTurnFailed
		final.Error = err.Error()
	}
	if recordErr := s.record(final); recordErr != nil {
		err = errors.Join(err, recordErr)
	}
	if saveErr := s.provider.sessions.saveTurnTime(s.id, turn, turnTime{StartedAt: started, EndedAt: final.At}); saveErr != nil {
		s.provider.deps.Log.Warn("Cursor turn time not saved", "session", s.id, "error", saveErr)
	}
	if meta, metaErr := s.provider.sessions.meta(s.id); metaErr == nil {
		s.mu.Lock()
		s.nativeSize, s.nativeTime = meta.SizeBytes, meta.UpdatedAt
		s.mu.Unlock()
	}
	return result, err
}

func prompt(req agent.TurnRequest, images bool) ([]cursoragent.Content, error) {
	out := []cursoragent.Content{}
	if req.Prompt != "" {
		out = append(out, cursoragent.Content{Type: "text", Text: req.Prompt})
	}
	for _, a := range req.Attachments {
		if a.IsImage() {
			if !images {
				return nil, errors.New("Cursor does not accept image prompts")
			}
			b, err := os.ReadFile(a.Path)
			if err != nil {
				return nil, err
			}
			out = append(out, cursoragent.Content{Type: "image", Data: base64.StdEncoding.EncodeToString(b), MimeType: a.MimeType})
		} else {
			out = append(out, cursoragent.Content{Type: "text", Text: fmt.Sprintf("Attached file %s: %s", a.Name, a.Path)})
		}
	}
	return out, nil
}
func (s *liveSession) record(e agent.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordLocked(e)
}
func (s *liveSession) recordLocked(e agent.Event) error {
	e.SessionID = s.id
	e.TurnID = s.historyTurn
	s.history = append(s.history, e)
	return nil
}
func (s *liveSession) emitLocked(e agent.Event) error {
	if s.io == nil && !s.replaying {
		return nil
	}
	// Replay has no original times; restoreTimes fills in the turn's own.
	if !s.replaying && e.At == 0 {
		e.At = time.Now().UnixMilli()
	}
	if err := s.recordLocked(e); err != nil {
		return err
	}
	if s.io != nil {
		s.io.Emit(e)
	}
	return nil
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
func (s *liveSession) reapable(cutoff time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed || (s.io == nil && s.lastActivity.Before(cutoff))
}
func (s *liveSession) Running() (agent.RunningTurn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.io == nil || s.closed {
		return agent.RunningTurn{}, false
	}
	return agent.RunningTurn{Ctx: s.turnCtx, TurnID: s.io.TurnID, ChatID: s.key, Cwd: s.cwd, Device: s.io.Device}, true
}
func (s *liveSession) ask(ctx context.Context, a agent.Approval) (json.RawMessage, error) {
	s.mu.Lock()
	stream, turnCtx := s.io, s.turnCtx
	s.mu.Unlock()
	if stream == nil || stream.Ask == nil {
		return nil, errors.New("no Cursor turn waiting for input")
	}
	request, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(turnCtx, cancel)
	defer func() { stop(); cancel() }()
	return stream.Ask(request, a)
}

func (s *liveSession) resetToolsLocked() {
	s.tools = map[string]cursoragent.ToolCall{}
	s.denied = map[string]bool{}
	s.finished = map[string]bool{}
}
func (s *liveSession) replayUserLocked(text string) {
	if n := len(s.history); n > 0 && s.history[n-1].Kind == agent.EventUserMessage {
		s.history[n-1].Text += text
		return
	}
	if s.historyTurn != "" {
		_ = s.recordLocked(agent.Event{Kind: agent.EventTurnFinished})
	}
	s.historyTurns++
	s.historyTurn = fmt.Sprintf("%s:%d", s.id, s.historyTurns)
	s.resetToolsLocked()
	_ = s.recordLocked(agent.Event{Kind: agent.EventTurnStarted})
	_ = s.recordLocked(agent.Event{Kind: agent.EventUserMessage, Text: text})
}
func (s *liveSession) finishReplayLocked() {
	if s.historyTurn != "" {
		_ = s.recordLocked(agent.Event{Kind: agent.EventTurnFinished})
	}
	s.replaying = false
	s.provider.sessions.restoreTimes(s.id, s.history)
}
