package store

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

// hashEvents folds the event stream into one value, and separately the value
// after the first `prefixLen` events, which tells an append from a rewrite.
// FNV-1a because a collision only costs a missed reload the next sweep fixes.
func hashEvents(events []agent.Event, prefixLen int) (full, prefix uint64) {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	mix := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= prime64
		}
		h ^= 0xff
		h *= prime64
	}

	prefix = offset64
	for i, e := range events {
		if i == prefixLen {
			prefix = h
		}
		mix(string(e.Kind))
		mix(e.TurnID)
		mix(e.Text)
		if e.Tool != nil {
			mix(e.Tool.CallID)
			mix(e.Tool.Name)
			mix(string(e.Tool.Input))
			mix(e.Tool.Output)
		}
	}
	if prefixLen >= len(events) {
		prefix = h
	}
	return h, prefix
}

// SyncBatch writes many sessions in one all-or-nothing transaction; a failed
// batch is simply reparsed. The listener hears each chat the files brought in
// or whose status or last turn they moved, which no hook or Manager reports.
func (s *Store) SyncBatch(entries []Entry) error {
	b, err := s.newBatch()
	if err != nil {
		return err
	}
	defer b.Close()

	fresh := false
	var changed []ChatID
	for _, e := range entries {
		listed, moved, err := b.syncEntry(e)
		if err != nil {
			return err
		}
		fresh = fresh || !listed
		if moved {
			changed = append(changed, ChatID(agent.ChatID(e.Meta.Agent, e.Meta.ID)))
		}
	}
	// A new chat is named in the same write that lists it.
	if fresh {
		renamed, err := s.assignVoiceHandles(b.tx)
		if err != nil {
			return err
		}
		changed = append(changed, renamed...)
	}
	if err := b.tx.Commit(); err != nil {
		return err
	}
	s.notify(Change{Changed: changed})
	return nil
}

// batch is one SyncBatch transaction and the statements it runs per session.
type batch struct {
	s   *Store
	tx  *sql.Tx
	now int64

	upsert, clear, prior, setTurn, setStatus, unresolve, dropEmptyMarks *sql.Stmt
}

func (s *Store) newBatch() (*batch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	b := &batch{s: s, tx: tx, now: time.Now().UnixMilli()}
	for _, st := range []struct {
		stmt  **sql.Stmt
		query string
	}{
		{&b.upsert, sessionUpsert},
		{&b.clear, `DELETE FROM events WHERE sid = ?`},
		{&b.prior, `SELECT event_count, content_hash, synced_at, updated_at, size_bytes, last_turn_key, last_turn_started_at, last_turn_finished_at
			FROM sessions WHERE agent = ? AND session_id = ?`},
		{&b.setTurn, `UPDATE sessions SET last_turn_key = ?, last_turn_started_at = ?, last_turn_finished_at = ? WHERE sid = ?`},
		// Same guard as SetStatus: a hook or the Manager may already have said
		// something newer about this chat, and the file must not roll that back.
		{&b.setStatus, `UPDATE sessions SET status = ?, status_at = ? WHERE sid = ? AND status_at < ?`},
		// The import's one write to the user's marks: a resolve older than a
		// prompt in the file is cleared, and the row goes once nothing is set.
		{&b.unresolve, `UPDATE state.chat_marks SET resolved_at = NULL WHERE agent = ? AND session_id = ? AND resolved_at < ?`},
		{&b.dropEmptyMarks, `DELETE FROM state.chat_marks WHERE agent = ? AND session_id = ? AND resolved_at IS NULL`},
	} {
		if *st.stmt, err = tx.Prepare(st.query); err != nil {
			b.Close()
			return nil, err
		}
	}
	return b, nil
}

// Close releases the statements and rolls back an uncommitted transaction.
func (b *batch) Close() {
	for _, st := range []*sql.Stmt{b.upsert, b.clear, b.prior, b.setTurn, b.setStatus, b.unresolve, b.dropEmptyMarks} {
		if st != nil {
			st.Close()
		}
	}
	b.tx.Rollback()
}

// syncEntry replaces one session's rows. listed is whether the chat was
// already cached; moved is whether its row reads differently: its first
// transcript, or a changed status or last turn.
func (b *batch) syncEntry(e Entry) (listed, moved bool, err error) {
	kind, id := string(e.Meta.Agent), e.Meta.ID
	// What we already had, so an append can be told from a rewrite.
	var oldCount int
	// SQLite integers are signed; a uint64 hash round-trips through int64.
	var storedHash, syncedAt, storedUpdated, storedSize int64
	var turnKey string
	var turnStarted, turnFinished sql.NullInt64
	switch err := b.prior.QueryRow(kind, id).Scan(&oldCount, &storedHash, &syncedAt, &storedUpdated, &storedSize,
		&turnKey, &turnStarted, &turnFinished); err {
	case nil:
		listed = true
	case sql.ErrNoRows:
	default:
		return false, false, err
	}
	// The sweep and a chat's flush parse the same file apart; the one that read
	// it first can write last. Its shorter rows would read as a rewrite.
	if syncedAt > 0 && readBefore(e, storedUpdated, storedSize, oldCount) {
		return listed, false, nil
	}
	// A row a hook placed ahead of its file has no reply or events until now;
	// at most the title and model write 1 took from the request.
	moved = syncedAt == 0

	full, prefix := hashEvents(e.Events, oldCount)
	// Growth over an unchanged prefix is an append the client keeps. A row
	// with no events yet has no history to invalidate.
	bump := 0
	if oldCount > len(e.Events) || (oldCount > 0 && prefix != uint64(storedHash)) {
		bump = 1
	}

	// Writes 2 and 3 in queries.go: the prompt reaching the file, then the
	// reply. RETURNING gives the surrogate key whether the row was new or not.
	var sid, searchHash int64
	var title string
	agentTitled := collapseSpace(e.Meta.Title) != ""
	if err := b.upsert.QueryRow(kind, id, e.Meta.Cwd, titleFor(e.Meta, e.Events), lastReply(e.Events), e.Meta.Path,
		createdAt(e.Meta, e.Events), e.Meta.UpdatedAt.UnixMilli(), e.Meta.SizeBytes, len(e.Events), int64(full), b.now,
		e.Meta.ContextUsed, e.Meta.ContextSize, e.Meta.Model,
		string(e.Meta.Settings.PermissionMode), e.Meta.Settings.Mode, e.Meta.Settings.ReasoningLevel, e.Meta.Settings.FastMode,
		lastMessageAt(e.Events), b.s.rev.Add(1), agentTitled, bump).Scan(&sid, &title, &searchHash); err != nil {
		return false, false, err
	}
	if _, err := b.clear.Exec(sid); err != nil {
		return false, false, err
	}
	if err := insertEvents(b.tx, sid, e.Events); err != nil {
		return false, false, err
	}
	if err := indexChat(b.tx, sid, title, e.Events, searchHash); err != nil {
		return false, false, err
	}
	// The same stamps a hook would have left, read off the file, so a chat
	// indexed cold times its last turn like one the host watched live.
	if key, started, finished := mergeTurn(turnKey, turnStarted, turnFinished, transcriptTurn(e.Events)); key != turnKey || started != turnStarted || finished != turnFinished {
		if _, err := b.setTurn.Exec(key, started, finished, sid); err != nil {
			return false, false, err
		}
		moved = true
	}
	status, at := transcriptStatus(e.Events)
	if status == agent.ChatUnknown {
		return listed, moved, nil
	}
	res, err := b.setStatus.Exec(status, at, sid, at)
	if err != nil {
		return false, false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		moved = true
	}
	// A prompt after the resolve reopens the chat, as a send from a device
	// does; the transcript is the only place a Codex CLI prompt shows up.
	if started := lastTurnStart(e.Events); started > 0 {
		if _, err := b.unresolve.Exec(kind, id, started); err != nil {
			return false, false, err
		}
		if _, err := b.dropEmptyMarks.Exec(kind, id); err != nil {
			return false, false, err
		}
	}
	return listed, moved, nil
}

// readBefore reports whether e is an older read of the file than the stored
// one: an earlier modification time or a smaller file, or the same file
// yielding fewer rows because it was read before the rest was written.
func readBefore(e Entry, storedUpdated, storedSize int64, storedCount int) bool {
	updated := e.Meta.UpdatedAt.UnixMilli()
	if updated != storedUpdated {
		return updated < storedUpdated
	}
	if e.Meta.SizeBytes != storedSize {
		return e.Meta.SizeBytes < storedSize
	}
	return len(e.Events) < storedCount
}

// transcriptStatus is the state the transcript records, and when: Codex writes
// its turn lifecycle into the rollout; Claude writes none, so reads unknown
// here and takes its status from hooks.
func transcriptStatus(events []agent.Event) (agent.ChatStatus, int64) {
	status, at := agent.ChatUnknown, int64(0)
	for _, e := range events {
		switch e.Kind {
		case agent.EventTurnStarted:
			status = agent.ChatWorking
		case agent.EventTurnFinished:
			status = agent.ChatIdle
		case agent.EventTurnFailed:
			status = agent.ChatFailed
			if e.Error == "aborted" {
				status = agent.ChatInterrupted
			}
		case agent.EventUserMessage:
			// A prompt after a failure is a turn the file has no lifecycle for
			// (Claude writes a failure, an API error, but no start): the
			// failure is past, and hooks say what is.
			if status == agent.ChatFailed || status == agent.ChatInterrupted {
				status, at = agent.ChatUnknown, 0
			}
			continue
		default:
			continue
		}
		at = e.At
	}
	if status == agent.ChatUnknown || at == 0 {
		return agent.ChatUnknown, 0
	}
	return status, at
}

// insertEvents writes rows in multi-VALUES statements: each statement is one
// cgo crossing and one VM entry, the dominant per-row cost.
const eventsPerStatement = 128

func insertEvents(tx *sql.Tx, sid int64, events []agent.Event) error {
	const cols = 7
	for start := 0; start < len(events); start += eventsPerStatement {
		end := start + eventsPerStatement
		if end > len(events) {
			end = len(events)
		}
		n := end - start

		var b strings.Builder
		b.WriteString(`INSERT INTO events (sid, idx, kind, turn, text, tool, at) VALUES `)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`(?,?,?,?,?,?,?)`)
		}

		args := make([]any, 0, n*cols)
		for i := start; i < end; i++ {
			tool, text := eventCols(events[i])
			var turn any
			if events[i].TurnID != "" {
				turn = events[i].TurnID
			}
			var at any
			if events[i].At != 0 {
				at = events[i].At
			}
			args = append(args, sid, i, string(events[i].Kind), turn, text, tool, at)
		}
		if _, err := tx.Exec(b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

func eventCols(e agent.Event) (tool any, text any) {
	if e.Tool != nil {
		if b, err := json.Marshal(e.Tool); err == nil {
			tool = string(b)
		}
	}
	if e.Text != "" {
		text = e.Text
	} else if e.Error != "" {
		// A failed turn's row is otherwise empty: the reason is what a client
		// shows, and how it tells a stop from a crash.
		text = e.Error
	}
	return
}

// MessagesOf is events in the shape a page carries, indexed from zero, for a
// transcript served straight from the provider's files rather than the cache.
func MessagesOf(events []agent.Event) []Message {
	out := make([]Message, 0, len(events))
	for i, e := range events {
		tool, text := eventCols(e)
		m := Message{Idx: i, Kind: string(e.Kind), Turn: e.TurnID, At: e.At}
		if s, ok := tool.(string); ok {
			m.Tool = s
		}
		if s, ok := text.(string); ok {
			m.Text = s
		}
		out = append(out, m)
	}
	return out
}

// Sync replaces one session's rows.
func (s *Store) Sync(meta session.Meta, events []agent.Event) error {
	return s.SyncBatch([]Entry{{meta, events}})
}

// Bulk trades durability away for a first full sync; a crash costs a reparse.
// Never leave it on for steady-state writes.
func (s *Store) Bulk(on bool) error {
	mode := "NORMAL"
	if on {
		mode = "OFF"
	}
	_, err := s.db.Exec("PRAGMA synchronous=" + mode)
	return err
}

// createdAt is when the chat began: the earliest timestamped line in the
// transcript. A transcript with no timestamps falls back to the file's mtime,
// which for a chat that has only just started is the same moment.
func createdAt(meta session.Meta, events []agent.Event) int64 {
	for _, e := range events {
		if e.At > 0 {
			return e.At
		}
	}
	return meta.UpdatedAt.UnixMilli()
}

// lastMessageAt is when the transcript's newest user message was sent, or 0.
func lastMessageAt(events []agent.Event) int64 {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == agent.EventUserMessage && events[i].At > 0 {
			return events[i].At
		}
	}
	return 0
}

// lastTurnStart is when the transcript's newest turn began, or 0.
func lastTurnStart(events []agent.Event) int64 {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == agent.EventTurnStarted {
			return events[i].At
		}
	}
	return 0
}

// transcriptTurnPrefix marks a turn observation read off the file rather than
// seen live. A hook or the Manager that saw the same turn is exact and takes
// the row over; the file only ever fills in for them.
const transcriptTurnPrefix = "transcript:"

// transcriptTurn is the newest turn as a hook reports it, keyed on the prompt's
// stamp so a reparse lands on the same turn. Codex writes lifecycle rows; for
// Claude the finish is the agent's last line, only as far as it has got.
func transcriptTurn(events []agent.Event) TurnObservation {
	start := -1
	for i := len(events) - 1; i >= 0 && start < 0; i-- {
		if events[i].Kind == agent.EventTurnStarted {
			start = i
		}
	}
	lifecycle := start >= 0
	for i := len(events) - 1; i >= 0 && start < 0; i-- {
		if events[i].Kind == agent.EventUserMessage {
			start = i
		}
	}
	if start < 0 || events[start].At == 0 {
		return TurnObservation{}
	}
	startedAt := time.UnixMilli(events[start].At)
	turn := TurnObservation{Key: transcriptTurnPrefix + strconv.FormatInt(events[start].At, 10), StartedAt: &startedAt}
	var finished int64
	for _, e := range events[start+1:] {
		if e.At == 0 {
			continue
		}
		switch e.Kind {
		case agent.EventTurnFinished, agent.EventTurnFailed:
			finished = e.At
		case agent.EventText, agent.EventReasoning, agent.EventToolCall, agent.EventToolResult:
			if !lifecycle {
				finished = e.At
			}
		}
	}
	if finished > 0 {
		finishedAt := time.UnixMilli(finished)
		turn.FinishedAt = &finishedAt
	}
	return turn
}
