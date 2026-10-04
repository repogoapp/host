package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/errkind"
)

var (
	// ErrNotFound is a chat this cache does not hold, which is routine: the
	// sweep may not have reached it.
	ErrNotFound = errkind.New(errkind.NotFound, "store: no such chat")

	// ErrInvalid is a malformed chat id, cursor, or query: the caller's mistake.
	ErrInvalid = errkind.New(errkind.Invalid, "store: invalid request")
)

// Reading for clients. Sync is a cursor, not a merge: the host is the only
// writer, `idx` makes a page idempotent, and `generation` bumps on a wholesale
// replace so a client restarts at 0 instead of splicing onto stale rows.

// ChatID is how a client names a chat: "<agent>:<session_id>". Never the
// integer `sid`, which is reassigned on a rebuild.
type ChatID string

func chatID(kind, sessionID string) ChatID { return ChatID(agent.ChatID(agent.Kind(kind), sessionID)) }

func (c ChatID) split() (kind, sessionID string, ok bool) {
	k, sessionID, ok := agent.SplitChatID(string(c))
	return string(k), sessionID, ok
}

// Kind is the agent the chat belongs to; empty when malformed.
func (c ChatID) Kind() agent.Kind {
	kind, _, _ := c.split()
	return agent.Kind(kind)
}

// SessionID is the provider's own id for the chat; empty when malformed.
func (c ChatID) SessionID() string {
	_, sessionID, _ := c.split()
	return sessionID
}

// parts is split with a malformed id as ErrInvalid.
func (c ChatID) parts() (kind, sessionID string, err error) {
	kind, sessionID, ok := c.split()
	if !ok {
		return "", "", fmt.Errorf("%w: malformed chat id %q", ErrInvalid, c)
	}
	return kind, sessionID, nil
}

// Chat is one row in the client's list.
type Chat struct {
	ID ChatID `json:"id"`

	// Which machine this chat lives on, stamped by the transport rather than
	// stored: identity belongs to the host, not a disposable cache. On every row
	// so a client can merge several hosts into one list.
	HostID string `json:"host_id,omitempty"`

	Agent              string           `json:"agent"`
	Title              string           `json:"title"`
	CWD                string           `json:"cwd"`
	CreatedAt          int64            `json:"created_at"`
	UpdatedAt          int64            `json:"updated_at"`
	ActivityAt         int64            `json:"activity_at"` // see sessions.activity_at in schema.go
	EventCount         int              `json:"event_count"`
	Generation         int64            `json:"generation"`
	Status             agent.ChatStatus `json:"status"`
	StatusAt           int64            `json:"status_at"`
	LastTurnStartedAt  *int64           `json:"last_turn_started_at"`
	LastTurnFinishedAt *int64           `json:"last_turn_finished_at"`

	// The opening of the agent's newest reply: the inbox row's preview line.
	LastReply string `json:"last_reply,omitempty"`

	// Derived from the newest saved user event for the inbox preview.
	LastUserMessagePreview string `json:"last_user_message_preview,omitempty"`

	// Where this row stands on the store's one revision counter; see sync.go.
	Rev int64 `json:"rev"`

	// When the user marked the chat's work done. Nil is live. A resolved chat
	// leaves the inbox but keeps its history; activity in it clears the mark.
	ResolvedAt *int64 `json:"resolved_at,omitempty"`

	// How full the context was on the newest request and the window it was
	// measured against. Omitted when unknown; Claude never writes a size, so
	// ContextUsed without ContextSize is normal there.
	ContextUsed int64 `json:"context_used,omitempty"`
	ContextSize int64 `json:"context_size,omitempty"`

	// The provider's id for the newest request's model; omitted when unknown.
	Model string `json:"model,omitempty"`
	// What a phone shows for Model; see Stamp.
	ModelLabel string `json:"model_label,omitempty"`

	// What the newest turn ran with, read from the transcript; omitted when
	// unknown. The same names and values chats.send takes.
	PermissionMode string `json:"permission_mode,omitempty"`
	Mode           string `json:"mode,omitempty"`
	ReasoningLevel string `json:"reasoning_level,omitempty"`
	FastMode       *bool  `json:"fast_mode,omitempty"`

	// The chat's spoken name for the voice agent; see voice.go. Nil once the
	// pool is spent on newer chats.
	VoiceHandle *VoiceHandle `json:"voice_handle,omitempty"`

	// How many turns wait in the chat's queue, and the version of that queue
	// (queue.go): a device refetches chats.queue only when the version moves,
	// since the row is resent on every flush of a running turn.
	QueuedCount int   `json:"queued_count,omitempty"`
	QueueRev    int64 `json:"queue_rev,omitempty"`

	// Under a search, the words around the hit in the chat's prompts and
	// replies, each matched word marked ⟪…⟫; empty when the title matched.
	Match string `json:"match,omitempty"`
}

// Stamp adds what the cache does not store, for a row leaving this host: the
// host it lives on, and what a phone shows for its model.
func (c *Chat) Stamp(host string, modelLabel func(agent.Kind, string) string) {
	c.HostID = host
	c.ModelLabel = modelLabel(agent.Kind(c.Agent), c.Model)
}

// Message is one normalized event.
type Message struct {
	Idx  int    `json:"idx"`
	Kind string `json:"kind"`
	Turn string `json:"turn,omitempty"`
	Text string `json:"text,omitempty"`
	Tool string `json:"tool,omitempty"`

	// Unix milliseconds; zero when the transcript line carried none.
	At int64 `json:"at,omitempty"`
}

// Page is a slice of one chat's transcript plus what the client needs to ask
// for the next one.
type Page struct {
	ChatID ChatID    `json:"chat_id"`
	Events []Message `json:"events"`

	// Where the conversation is happening. A project is addressed by (host, cwd);
	// HostID is stamped by the transport, not the cache.
	HostID string `json:"host_id,omitempty"`
	Cwd    string `json:"cwd"`
	Agent  string `json:"agent"`

	// Generation the rows came from. A client whose held generation differs
	// must drop everything it has for this chat and refetch from index 0.
	Generation int64 `json:"generation"`

	// Total in the transcript, so a client can show progress and know whether
	// it has reached the end without a second round trip.
	EventCount int `json:"event_count"`

	// NextIdx is what to send as `since_idx` next time. Equal to EventCount
	// once the client is caught up.
	NextIdx int `json:"next_idx"`

	// FirstIdx is the lowest index in this page, and what to send as
	// `before_idx` to walk further back. A chat is read from the bottom, so
	// paging upward needs its own cursor — NextIdx only goes forward.
	FirstIdx int `json:"first_idx"`

	// HasBefore is false once index 0 is on screen, so a client knows when to
	// stop asking rather than discovering it from an empty page.
	HasBefore bool `json:"has_before"`
}

// ChatQuery is how a client wants its inbox read: which pile, in what order,
// over which projects. The same shape RepoGo's inbox prefs describe, so the
// two apps' lists can be told apart by data alone.
type ChatQuery struct {
	Limit int

	// Cursor is the opaque token the previous page returned; empty starts over.
	Cursor string

	// Cwd narrows to one project; Cwds to a set (the inbox's project filter).
	// Empty is every project on the host.
	Cwd  string
	Cwds []string

	// Resolved picks the done pile (true), the live one (false), or both (nil).
	Resolved *bool

	// Agent narrows to one agent's chats; empty is every agent.
	Agent string

	// Search narrows to chats whose title, prompts and replies hold every
	// word of the text (search.go); empty matches everything.
	Search string

	Sort  ChatSort
	Order ChatOrder
}

type ChatSort string

const (
	// The chat's activity_at: the last prompt, or the reply ending after it.
	SortUpdated ChatSort = "updated"
	// The chat's created_at, which never moves, so a row keeps its place.
	SortCreated ChatSort = "created"
	SortTitle   ChatSort = "title"
)

type ChatOrder string

const (
	OrderDesc ChatOrder = "desc"
	OrderAsc  ChatOrder = "asc"
)

// ChatPage is one page of the inbox plus the token for the next.
type ChatPage struct {
	Chats      []Chat
	NextCursor string
}

// chatSelect reads chat rows with what state.db holds for them (marks, voice
// handles, the queue's count and version), for queryChats.
const chatSelect = `
SELECT s.agent, s.session_id, s.title, s.last_reply,
  COALESCE((SELECT e.text FROM events e WHERE e.sid = s.sid AND e.kind = 'user_message' ORDER BY e.idx DESC LIMIT 1), ''), s.cwd, s.created_at, s.updated_at, s.activity_at, s.event_count, s.generation, s.context_used, s.context_size, s.model, s.permission_mode, s.mode, s.reasoning_level, s.fast_mode, s.status, s.status_at, s.last_turn_started_at, s.last_turn_finished_at, m.resolved_at, s.rev, v.prefix, v.name,
  (SELECT COUNT(*) FROM state.queued_turns t WHERE t.chat_id = s.agent || ':' || s.session_id), COALESCE(q.rev, 0)
FROM sessions s LEFT JOIN state.chat_marks m ON m.agent = s.agent AND m.session_id = s.session_id
LEFT JOIN state.voice_handles v ON v.agent = s.agent AND v.session_id = s.session_id
LEFT JOIN state.chat_queues q ON q.chat_id = s.agent || ':' || s.session_id`

func (s *Store) queryChats(query string, args ...any) ([]Chat, error) {
	return readChats(s.db, query, args...)
}

// readChats is queryChats on a connection or a transaction.
func readChats(db querier, query string, args ...any) ([]Chat, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read chats: %w", err)
	}
	defer rows.Close()

	var out []Chat
	for rows.Next() {
		var c Chat
		var agent, sessionID string
		var voicePrefix, voiceName sql.NullString
		if err := rows.Scan(&agent, &sessionID, &c.Title, &c.LastReply, &c.LastUserMessagePreview, &c.CWD,
			&c.CreatedAt, &c.UpdatedAt, &c.ActivityAt, &c.EventCount, &c.Generation, &c.ContextUsed, &c.ContextSize, &c.Model, &c.PermissionMode, &c.Mode, &c.ReasoningLevel, &c.FastMode, &c.Status, &c.StatusAt,
			&c.LastTurnStartedAt, &c.LastTurnFinishedAt, &c.ResolvedAt, &c.Rev, &voicePrefix, &voiceName,
			&c.QueuedCount, &c.QueueRev); err != nil {
			return nil, err
		}
		c.LastUserMessagePreview = previewOf(c.LastUserMessagePreview)
		c.ID, c.Agent = chatID(agent, sessionID), agent
		c.VoiceHandle = voiceHandleFrom(voicePrefix, voiceName)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Chat pages hold defaultChats rows unless asked, and never more than maxChats.
const (
	defaultChats = 50
	maxChats     = 500
)

// withDefaults fills an unset sort, order and limit, refuses unknown ones, and
// trims the search.
func (q ChatQuery) withDefaults() (ChatQuery, error) {
	q.Search = strings.TrimSpace(q.Search)
	q.Limit = clamp(q.Limit, defaultChats, maxChats)
	switch q.Sort {
	case "":
		q.Sort = SortUpdated
	case SortUpdated, SortCreated, SortTitle:
	default:
		return q, fmt.Errorf("%w: unknown sort %q", ErrInvalid, q.Sort)
	}
	switch q.Order {
	case "":
		q.Order = OrderDesc
	case OrderDesc, OrderAsc:
	default:
		return q, fmt.Errorf("%w: unknown order %q", ErrInvalid, q.Order)
	}
	return q, nil
}

// cursorAt is the keyset position of one row under q: session id first, since
// neither it nor a collapsed title holds a newline.
func (q ChatQuery) cursorAt(c Chat) string {
	switch q.Sort {
	case SortTitle:
		return c.ID.SessionID() + "\n" + c.Title
	case SortCreated:
		return c.ID.SessionID() + "\n" + strconv.FormatInt(c.CreatedAt, 10)
	default:
		return c.ID.SessionID() + "\n" + strconv.FormatInt(c.ActivityAt, 10)
	}
}

// Chats lists conversations under a query, paged by keyset rather than offset
// so a chat that changes mid-scroll does not skip or repeat rows. session_id
// breaks ties, or two rows sharing a key could loop the cursor forever.
func (s *Store) Chats(q ChatQuery) (ChatPage, error) {
	q, err := q.withDefaults()
	if err != nil {
		return ChatPage{}, err
	}
	limit := q.Limit
	key := "s.activity_at"
	switch q.Sort {
	case SortTitle:
		key = "s.title"
	case SortCreated:
		key = "s.created_at"
	}
	cmp, dir := "<", "DESC"
	if q.Order == OrderAsc {
		cmp, dir = ">", "ASC"
	}

	query := chatSelect
	args := []any{}
	where := []string{}
	switch {
	case q.Resolved == nil:
	case *q.Resolved:
		where = append(where, `m.resolved_at IS NOT NULL`)
	default:
		where = append(where, `m.resolved_at IS NULL`)
	}
	if q.Cursor != "" {
		sessionID, keyValue, ok := strings.Cut(q.Cursor, "\n")
		if !ok {
			return ChatPage{}, fmt.Errorf("%w: malformed cursor", ErrInvalid)
		}
		var after any = keyValue
		if q.Sort != SortTitle {
			n, err := strconv.ParseInt(keyValue, 10, 64)
			if err != nil {
				return ChatPage{}, fmt.Errorf("%w: malformed cursor", ErrInvalid)
			}
			after = n
		}
		// Row-value comparison: strictly past the last row in sort order, ties
		// broken the same way the ORDER BY does.
		where = append(where, fmt.Sprintf(`(%s, s.session_id) %s (?, ?)`, key, cmp))
		args = append(args, after, sessionID)
	}
	if q.Cwd != "" {
		where = append(where, `s.cwd = ?`)
		args = append(args, q.Cwd)
	}
	if len(q.Cwds) > 0 {
		where = append(where, `s.cwd IN (`+marks(len(q.Cwds))+`)`)
		args = append(args, anys(q.Cwds)...)
	}
	if q.Agent != "" {
		where = append(where, `s.agent = ?`)
		args = append(args, q.Agent)
	}
	match := ""
	if q.Search != "" {
		// Text with no words in it, punctuation alone, matches no chat.
		if match = searchMatch(q.Search); match == "" {
			return ChatPage{Chats: []Chat{}}, nil
		}
		where = append(where, `s.sid IN (SELECT rowid FROM chat_search WHERE chat_search MATCH ?)`)
		args = append(args, match)
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += fmt.Sprintf(` ORDER BY %s %s, s.session_id %s LIMIT ?`, key, dir, dir)
	args = append(args, limit)

	out, err := s.queryChats(query, args...)
	if err != nil {
		return ChatPage{}, err
	}
	if match != "" {
		if err := s.addMatches(out, match); err != nil {
			return ChatPage{}, err
		}
	}
	page := ChatPage{Chats: out}
	if page.Chats == nil {
		page.Chats = []Chat{}
	}
	// A short page is the end; a full one may be, and the client finds out by
	// asking, which costs one empty round trip only at the very bottom.
	if n := len(out); n == limit {
		page.NextCursor = q.cursorAt(out[n-1])
	}
	return page, nil
}

// Neighbors is the row before and after one chat under a query: what the
// transcript's arrows step to. Each side is a one-row page from the chat's own
// sort key, so it stays exact however the list has moved. Nil with nothing there.
func (s *Store) Neighbors(id ChatID, q ChatQuery) (previous, next *Chat, err error) {
	chat, err := s.Info(id)
	if err != nil {
		return nil, nil, err
	}
	if q, err = q.withDefaults(); err != nil {
		return nil, nil, err
	}
	q.Cursor, q.Limit = q.cursorAt(chat), 1

	one := func(q ChatQuery) (*Chat, error) {
		page, err := s.Chats(q)
		if err != nil || len(page.Chats) == 0 {
			return nil, err
		}
		return &page.Chats[0], nil
	}
	if next, err = one(q); err != nil {
		return nil, nil, err
	}
	// The row before is the first row past the chat with the order flipped.
	flipped := q
	flipped.Order = OrderAsc
	if q.Order == OrderAsc {
		flipped.Order = OrderDesc
	}
	if previous, err = one(flipped); err != nil {
		return nil, nil, err
	}
	return previous, next, nil
}

// Roots is every distinct folder a chat has run in or the user picked, most
// recent first: the filesystem family's allowed set.
func (s *Store) Roots() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT cwd FROM (
		   SELECT cwd, activity_at AS at FROM sessions WHERE cwd != ''
		   UNION ALL
		   SELECT path, picked_at FROM state.project_marks WHERE picked_at IS NOT NULL)
		 GROUP BY cwd ORDER BY MAX(at) DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: roots: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var cwd string
		if err := rows.Scan(&cwd); err != nil {
			return nil, err
		}
		out = append(out, cwd)
	}
	return out, rows.Err()
}

// Info returns one chat's row without any events — the address a caller needs
// to act on it: which agent, which folder, which provider session to resume.
func (s *Store) Info(id ChatID) (Chat, error) {
	agent, sessionID, err := id.parts()
	if err != nil {
		return Chat{}, err
	}
	chats, err := s.queryChats(chatSelect+` WHERE s.agent = ? AND s.session_id = ?`, agent, sessionID)
	if err != nil {
		return Chat{}, err
	}
	if len(chats) == 0 {
		return Chat{}, fmt.Errorf("%w: chat %q", ErrNotFound, id)
	}
	return chats[0], nil
}

// Tail returns the last `limit` events, where a transcript opens; paging
// forward from 0 would need a dozen round trips to reach the end.
func (s *Store) Tail(id ChatID, limit int) (Page, error) {
	return s.backward(id, -1, limit)
}

// Before returns the `limit` events immediately preceding beforeIdx — what
// scrolling up asks for.
func (s *Store) Before(id ChatID, beforeIdx, limit int) (Page, error) {
	if beforeIdx <= 0 {
		// Nothing precedes index 0. Answering with the tail instead would
		// silently jump the user back to the bottom.
		page, _, err := s.header(id)
		return page, err
	}
	return s.backward(id, beforeIdx, limit)
}

// Messages returns one chat's events from sinceIdx forward.
func (s *Store) Messages(id ChatID, sinceIdx, limit int) (Page, error) {
	page, sid, err := s.header(id)
	if err != nil {
		return Page{}, err
	}
	sinceIdx = max(sinceIdx, 0)
	page.Events, err = s.events(eventSelect+` AND idx >= ? ORDER BY idx LIMIT ?`, sid, sinceIdx, pageLimit(limit))
	if err != nil {
		return Page{}, err
	}
	page.NextIdx, page.FirstIdx = sinceIdx, sinceIdx
	if n := len(page.Events); n > 0 {
		page.FirstIdx, page.NextIdx = page.Events[0].Idx, page.Events[n-1].Idx+1
	}
	page.HasBefore = page.FirstIdx > 0
	return page, nil
}

// ToolCalls returns the call and result rows of one chat's tool calls named
// by callIDs, in transcript order: what a tool sheet shows.
func (s *Store) ToolCalls(id ChatID, callIDs []string) ([]Message, error) {
	_, sid, err := s.header(id)
	if err != nil || len(callIDs) == 0 {
		return []Message{}, err
	}
	args := []any{sid}
	for _, callID := range callIDs {
		args = append(args, callID)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(callIDs)), ",")
	return s.events(eventSelect+` AND kind IN ('tool_call', 'tool_result')
		AND json_extract(tool, '$.call_id') IN (`+marks+`) ORDER BY idx`, args...)
}

// header loads a chat's counters and surrogate key without any events.
func (s *Store) header(id ChatID) (Page, int64, error) {
	agent, sessionID, err := id.parts()
	if err != nil {
		return Page{}, 0, err
	}
	page := Page{ChatID: id, Agent: agent}
	var sid int64
	err = s.db.QueryRow(
		`SELECT sid, generation, event_count, cwd FROM sessions WHERE agent = ? AND session_id = ?`,
		agent, sessionID).Scan(&sid, &page.Generation, &page.EventCount, &page.Cwd)
	if err == sql.ErrNoRows {
		return Page{}, 0, fmt.Errorf("%w: chat %q", ErrNotFound, id)
	}
	if err != nil {
		return Page{}, 0, fmt.Errorf("store: load chat: %w", err)
	}
	page.NextIdx = page.EventCount
	return page, sid, nil
}

// backward selects the last `limit` events below beforeIdx, or of the whole
// transcript when negative. Selected DESC so SQLite stops early, then reversed.
func (s *Store) backward(id ChatID, beforeIdx, limit int) (Page, error) {
	page, sid, err := s.header(id)
	if err != nil {
		return Page{}, err
	}
	query := eventSelect
	args := []any{sid}
	if beforeIdx >= 0 {
		query += ` AND idx < ?`
		args = append(args, beforeIdx)
	}
	query += ` ORDER BY idx DESC LIMIT ?`
	args = append(args, pageLimit(limit))
	if page.Events, err = s.events(query, args...); err != nil {
		return Page{}, err
	}
	slices.Reverse(page.Events)
	if page.Events, err = s.toTurnStart(sid, page.Events); err != nil {
		return Page{}, err
	}

	if len(page.Events) == 0 {
		page.FirstIdx = max(beforeIdx, 0)
		return page, nil
	}
	page.FirstIdx = page.Events[0].Idx
	page.NextIdx = page.Events[len(page.Events)-1].Idx + 1
	page.HasBefore = page.FirstIdx > 0
	return page, nil
}

// maxTurnReach bounds how far toTurnStart reaches back; a turn longer than
// this still opens mid-reply rather than shipping it whole.
const maxTurnReach = 1000

// toTurnStart widens a backward page so it opens on its first reply's prompt
// or turn_started row. Cut mid-reply, the client holds a reply it cannot time
// ("Worked for …") until the next page up brings the rows that start it.
func (s *Store) toTurnStart(sid int64, events []Message) ([]Message, error) {
	if len(events) == 0 {
		return events, nil
	}
	first := events[0].Idx
	if kind := agent.EventKind(events[0].Kind); kind == agent.EventUserMessage || kind == agent.EventTurnStarted {
		return events, nil
	}
	var start int
	err := s.db.QueryRow(
		`SELECT idx FROM events WHERE sid = ? AND idx < ? AND idx >= ? AND kind IN (?, ?)
		 ORDER BY idx DESC LIMIT 1`,
		sid, first, first-maxTurnReach, string(agent.EventUserMessage), string(agent.EventTurnStarted)).Scan(&start)
	if err == sql.ErrNoRows {
		return events, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: find turn start: %w", err)
	}
	head, err := s.events(eventSelect+` AND idx >= ? AND idx < ? ORDER BY idx`, sid, start, first)
	if err != nil {
		return nil, err
	}
	return append(head, events...), nil
}

// Transcript pages hold defaultEvents rows unless asked, and never more than maxEvents.
const (
	defaultEvents = 200
	maxEvents     = 1000
)

func pageLimit(limit int) int { return clamp(limit, defaultEvents, maxEvents) }

// clamp is n, or fallback when unset, and never past most.
func clamp(n, fallback, most int) int {
	if n <= 0 {
		return fallback
	}
	return min(n, most)
}

// eventSelect reads one chat's events in Message's column order; callers append
// the rest of the WHERE clause.
const eventSelect = `SELECT idx, kind, COALESCE(turn,''), COALESCE(text,''), COALESCE(tool,''), COALESCE(at,0)
	FROM events WHERE sid = ?`

func (s *Store) events(query string, args ...any) ([]Message, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read events: %w", err)
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Idx, &m.Kind, &m.Turn, &m.Text, &m.Tool, &m.At); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
