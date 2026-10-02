// Package chat is what a device can do to a chat: read it, send into it, start
// one, resolve, delete, and ship it. The chats.* methods are its wire.
package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/chatsync"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
)

// Sender queues a turn: the agent Manager, narrowed to what a chat needs.
type Sender interface {
	Send(req agent.TurnRequest) (agent.TurnStatus, error)
	Steer(req agent.TurnRequest) (agent.TurnStatus, bool, error)
	List() []agent.TurnStatus
}

// Transcripts is the provider-file side of a chat: its session files and the
// subagents its calls started.
type Transcripts interface {
	Delete(sessionID string) error
	Rename(sessionID, title string) error
	Subagent(sessionID, callID string) (session.Subagent, error)
}

// Deps is everything a chat touches. Every field is required.
type Deps struct {
	// Self stamps which host these chats live on: two hosts can hold the same cwd.
	Self device.ID

	// ModelLabel names a row's model for the phone, from the agent's catalog.
	ModelLabel func(agent.Kind, string) string

	// Cache is not the copy of record, the provider files are, so every
	// method here tolerates it being stale.
	Cache       *store.Store
	Sender      Sender
	Sync        *chatsync.Syncer
	Transcripts Transcripts

	// Files contains a path before a chat starts in it.
	Files files.Container
}

// Service is every chat workflow over its Deps.
type Service struct {
	d Deps
}

// New refuses Deps with a field unset rather than failing on first use.
func New(d Deps) (*Service, error) {
	var missing []string
	for _, f := range []struct {
		name  string
		unset bool
	}{
		{"Self", d.Self == ""}, {"ModelLabel", d.ModelLabel == nil}, {"Cache", d.Cache == nil}, {"Sender", d.Sender == nil}, {"Sync", d.Sync == nil},
		{"Transcripts", d.Transcripts == nil}, {"Files", d.Files == nil},
	} {
		if f.unset {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("chat: missing %s", strings.Join(missing, ", "))
	}
	return &Service{d: d}, nil
}

// Self is the host these chats live on.
func (s *Service) Self() device.ID { return s.d.Self }

// List is one page of chats. refresh re-sweeps the disk first: the periodic
// sweep is 30s behind, and pull-to-refresh must not be.
func (s *Service) List(ctx context.Context, q store.ChatQuery, refresh bool) (store.ChatPage, error) {
	if refresh {
		s.d.Sync.Once(ctx)
	}
	page, err := s.d.Cache.Chats(q)
	for i := range page.Chats {
		s.stamp(&page.Chats[i])
	}
	return page, err
}

// Info is one chat's row, stamped with this host: the cache does not know
// which device it is, and a row without a host cannot be acted on.
func (s *Service) Info(id store.ChatID) (store.Chat, error) {
	chat, err := s.d.Cache.Info(id)
	s.stamp(&chat)
	return chat, err
}

func (s *Service) stamp(c *store.Chat) { c.Stamp(string(s.d.Self), s.d.ModelLabel) }

// Queue is one chat's queued turns, read from state.db, where the Manager
// saves every change to them.
func (s *Service) Queue(id store.ChatID) (store.Queue, error) { return s.d.Cache.Queue(id) }

// Neighbors is the row before and after one chat under the query the list was
// read with: the transcript's up and down arrows, exact as rows move.
func (s *Service) Neighbors(id store.ChatID, q store.ChatQuery) (previous, next *store.Chat, err error) {
	previous, next, err = s.d.Cache.Neighbors(id, q)
	for _, c := range []*store.Chat{previous, next} {
		if c != nil {
			s.stamp(c)
		}
	}
	return previous, next, err
}

// Page selects a transcript read. A transcript opens at the bottom and pages
// upward, so the two directions need separate cursors; absent both, this is
// the live push cursor's read.
type Page struct {
	SinceIdx  int
	Limit     int
	Tail      bool
	BeforeIdx *int
}

func (s *Service) Messages(id store.ChatID, p Page) (store.Page, error) {
	var page store.Page
	var err error
	switch {
	case p.Tail:
		page, err = s.d.Cache.Tail(id, p.Limit)
	case p.BeforeIdx != nil:
		page, err = s.d.Cache.Before(id, *p.BeforeIdx, p.Limit)
	default:
		page, err = s.d.Cache.Messages(id, p.SinceIdx, p.Limit)
	}
	page.HostID = string(s.d.Self)
	return page, err
}

// ToolCalls is the call and result rows of the tool calls a sheet shows.
func (s *Service) ToolCalls(id store.ChatID, callIDs []string) ([]store.Message, error) {
	if id.SessionID() == "" || len(callIDs) == 0 {
		return nil, fmt.Errorf("%w: chat_id and call_ids are required", errkind.ErrInvalid)
	}
	return s.d.Cache.ToolCalls(id, callIDs)
}

// Subagent is the transcript of the subagent the call callID started. Read
// from the provider's files on each call and not cached: it is opened rarely
// and only from its row.
func (s *Service) Subagent(id store.ChatID, callID string) (session.Subagent, error) {
	if id.SessionID() == "" || strings.TrimSpace(callID) == "" {
		return session.Subagent{}, fmt.Errorf("%w: chat_id and call_id are required", errkind.ErrInvalid)
	}
	return s.d.Transcripts.Subagent(id.SessionID(), callID)
}

// Turn is what chats.send and chats.start both run, in the shape a queued
// turn keeps: its prompt, its settings and its files. Per-turn, not stored;
// an empty setting is the agent's default.
type Turn struct {
	Prompt      string           `json:"prompt"`
	Config      agent.TurnConfig `json:"config"`
	Attachments []agent.Upload   `json:"attachments"`

	// Device is the caller, set from its connection; see agent.TurnRequest.
	Device device.ID `json:"-"`
}

// check runs before the turn is queued, so an empty prompt never reaches an
// agent.
func (t Turn) check() error {
	if strings.TrimSpace(t.Prompt) == "" && len(t.Attachments) == 0 {
		return fmt.Errorf("%w: prompt is empty", errkind.ErrInvalid)
	}
	return nil
}

// enqueue saves the attachments and queues req with this turn's settings,
// or steers it into the chat's running turn.
func (s *Service) enqueue(t Turn, req agent.TurnRequest, steer bool) (agent.TurnStatus, bool, error) {
	attachments, err := agent.SaveUploads(t.Attachments)
	if err != nil {
		return agent.TurnStatus{}, false, err
	}
	req.Prompt, req.Config, req.Attachments, req.Device = t.Prompt, t.Config, attachments, t.Device
	if steer {
		return s.d.Sender.Steer(req)
	}
	status, err := s.d.Sender.Send(req)
	return status, false, err
}

// Send runs a turn in an existing chat. The reply arrives on the push lane like
// any other append to the session file chatlive is tailing.
func (s *Service) Send(id store.ChatID, t Turn) (agent.TurnStatus, error) {
	status, _, err := s.send(id, t, false)
	return status, err
}

// Steer adds t to the chat's running turn when its agent reads input
// mid-turn, and otherwise sends it as Send does; steered says which.
func (s *Service) Steer(id store.ChatID, t Turn) (status agent.TurnStatus, steered bool, err error) {
	return s.send(id, t, true)
}

func (s *Service) send(id store.ChatID, t Turn, steer bool) (agent.TurnStatus, bool, error) {
	if err := t.check(); err != nil {
		return agent.TurnStatus{}, false, err
	}
	// Cwd and agent come from the chat, never the caller; that is why this is
	// remote-reachable and turns.create is not.
	info, err := s.d.Cache.Info(id)
	if err != nil {
		return agent.TurnStatus{}, false, err
	}
	status, steered, err := s.enqueue(t, agent.TurnRequest{
		ChatID: string(id),
		Cwd:    info.CWD,
		Agent:  agent.Kind(info.Agent),
		// Continue the provider's own conversation; otherwise a reply forks the chat.
		SessionID: id.SessionID(),
	}, steer)
	if err != nil {
		return agent.TurnStatus{}, false, err
	}
	// Sending into a resolved chat reopens it: nobody types "unresolve" first.
	// Unconditional, unlike the hook path's replay guard: this is the user's act.
	if info.ResolvedAt != nil {
		// Fail-soft: the turn is queued, and the next sweep reopens the row.
		_ = s.d.Cache.SetResolved(id, false, time.Now())
	}
	return status, steered, nil
}

// Start is where a new chat runs: the project folder itself.
type Start struct {
	Path  string
	Agent string
	// Title names the chat; empty leaves it to the prompt's opening words.
	Title string
}

// Start queues a new chat's first turn in the project folder. The chat id
// follows on turns.get once the agent opens it.
func (s *Service) Start(where Start, t Turn) (agent.TurnStatus, error) {
	if err := t.check(); err != nil {
		return agent.TurnStatus{}, err
	}
	if where.Agent == "" {
		return agent.TurnStatus{}, fmt.Errorf("%w: agent is empty", errkind.ErrInvalid)
	}
	path, err := s.d.Files.Contain(where.Path)
	if err != nil {
		return agent.TurnStatus{}, err
	}
	title := collapseTitle(where.Title)
	if utf8.RuneCountInString(title) > titleMaxRunes {
		return agent.TurnStatus{}, fmt.Errorf("%w: the title is over %d characters", errkind.ErrInvalid, titleMaxRunes)
	}
	// No chat id yet: the Manager queues the turn under a placeholder and
	// renames it once the provider opens the session.
	status, _, err := s.enqueue(t, agent.TurnRequest{Cwd: path, Agent: agent.Kind(where.Agent), Title: title}, false)
	return status, err
}

// Resolve marks a chat's work done, or takes it back. Resolved chats leave the
// live list without being deleted; the done pile is a list with resolved=true.
func (s *Service) Resolve(id store.ChatID, resolved bool) (store.Chat, error) {
	if _, err := s.d.Cache.Info(id); err != nil {
		return store.Chat{}, err
	}
	if err := s.d.Cache.SetResolved(id, resolved, time.Now()); err != nil {
		return store.Chat{}, err
	}
	return s.Info(id)
}

// Update is what a user changes on a chat; a nil field is left as it is.
type Update struct {
	Title *string `json:"title"`
}

// titleMaxRunes keeps a title to what a list row and the CLI's picker show.
const titleMaxRunes = 120

// Update applies the fields the caller set. A title is written to the
// provider's own files, so `claude --resume` and Codex's picker show it too.
func (s *Service) Update(id store.ChatID, u Update) (store.Chat, error) {
	if _, err := s.d.Cache.Info(id); err != nil {
		return store.Chat{}, err
	}
	if u.Title != nil {
		if err := s.setTitle(id, *u.Title); err != nil {
			return store.Chat{}, err
		}
	}
	return s.Info(id)
}

func (s *Service) setTitle(id store.ChatID, title string) error {
	title = collapseTitle(title)
	if title == "" {
		return fmt.Errorf("%w: the title is empty", errkind.ErrInvalid)
	}
	if utf8.RuneCountInString(title) > titleMaxRunes {
		return fmt.Errorf("%w: the title is over %d characters", errkind.ErrInvalid, titleMaxRunes)
	}
	if err := s.d.Transcripts.Rename(id.SessionID(), title); err != nil {
		return err
	}
	return s.d.Cache.SetTitle(id, title)
}

// collapseTitle puts a title on one line, the way a list row shows it.
func collapseTitle(title string) string { return strings.Join(strings.Fields(title), " ") }

// staleActivity is how long a chat's active status holds without a new word.
// A CLI run outside this host shows only through hooks and file writes, and
// one that exits mid-turn never says so; that must not make a chat undeletable.
const staleActivity = 2 * time.Minute

// Delete removes a chat for good: the provider's session files, then the
// cache row. A chat with a turn in flight is refused; stop it first.
func (s *Service) Delete(id store.ChatID) error {
	chat, err := s.d.Cache.Info(id)
	if err != nil {
		return err
	}
	fresh := chat.Status.Active() && time.Since(time.UnixMilli(chat.StatusAt)) < staleActivity
	if fresh || s.running(id) {
		return fmt.Errorf("%w: the chat has a turn in progress; stop it first", errkind.ErrInvalid)
	}
	if err := s.d.Transcripts.Delete(id.SessionID()); err != nil && !errors.Is(err, session.ErrNotFound) {
		return err
	}
	return s.d.Cache.Delete(id)
}

// running reports whether this host has a turn queued or running in the chat.
func (s *Service) running(id store.ChatID) bool {
	for _, t := range s.d.Sender.List() {
		if t.ChatID == string(id) && (t.State == agent.StateQueued || t.State == agent.StateRunning) {
			return true
		}
	}
	return false
}
