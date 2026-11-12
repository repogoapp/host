package store

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

// row is one event as its events row stores it. chain is the hash of every
// row up to and including this one, so two transcripts agree on their first
// n rows exactly when their chains agree at row n-1.
type row struct {
	kind  string
	turn  any
	text  any
	tool  any
	at    any
	chain int64
}

// emptyChain is the chain before any row: what a transcript with no events
// stores as its content_hash.
const emptyChain = -3750763034362895579 // offset64 as an int64

// FNV-1a, 64-bit.
const (
	offset64 = 14695981039346656037
	prime64  = 1099511628211
)

// eventRows is events as rows, chained. The hash covers each column as
// stored, so a row changed in any column it keeps breaks the chain there.
// FNV-1a because a collision only costs a row left stale until it changes.
func eventRows(events []agent.Event) []row {
	h := uint64(offset64)
	mix := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= prime64
		}
		h ^= 0xff
		h *= prime64
	}
	// nil and "" are different columns, NULL and empty text.
	mixCol := func(v any) {
		switch v := v.(type) {
		case string:
			mix("s" + v)
		case int64:
			mix("i" + strconv.FormatInt(v, 10))
		default:
			mix("n")
		}
	}

	rows := make([]row, len(events))
	for i, e := range events {
		r := row{kind: string(e.Kind)}
		r.tool, r.text = eventCols(e)
		if e.TurnID != "" {
			r.turn = e.TurnID
		}
		if e.At != 0 {
			r.at = e.At
		}
		mix(r.kind)
		mixCol(r.turn)
		mixCol(r.text)
		mixCol(r.tool)
		mixCol(r.at)
		r.chain = int64(h)
		rows[i] = r
	}
	return rows
}

// chainOf is the chain through the first n rows.
func chainOf(rows []row, n int) int64 {
	if n == 0 {
		return emptyChain
	}
	return rows[n-1].chain
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
	// A new chat is numbered in the same write that lists it.
	if fresh {
		if err := s.assignVoiceNumbers(b.tx); err != nil {
			return err
		}
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

	upsert, truncate, chainAt, prior, setTurn, setStatus, unresolve, dropEmptyMarks *sql.Stmt
	releaseModel, resetModel, markStale                                             *sql.Stmt
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
		{&b.truncate, `DELETE FROM events WHERE sid = ? AND idx >= ?`},
		{&b.chainAt, `SELECT chain FROM events WHERE sid = ? AND idx = ?`},
		{&b.markStale, `UPDATE sessions SET search_stale = 1 WHERE sid = ?`},
		{&b.prior, `SELECT sid, event_count, content_hash, synced_at, updated_at, size_bytes, last_turn_key, last_turn_started_at, last_turn_finished_at,
			model_turn_key FROM sessions WHERE agent = ? AND session_id = ?`},
		{&b.setTurn, `UPDATE sessions SET last_turn_key = ?, last_turn_started_at = ?, last_turn_finished_at = ? WHERE sid = ?`},
		{&b.releaseModel, `UPDATE sessions SET model_turn_key = '' WHERE sid = ?`},
		{&b.resetModel, `UPDATE sessions SET model_at = 0 WHERE agent = ? AND session_id = ?`},
		// Same guard as SetStatus: a hook or the Manager may already have said
		// something newer about this chat, and the file must not roll that back.
		{&b.setStatus, `UPDATE sessions SET status = ?, status_at = ? WHERE sid = ? AND status_at < ?`},
		// The import's one write to the user's marks: a resolve older than a
		// prompt in the file is cleared, and the row goes once nothing is set.
		{&b.unresolve, `UPDATE state.chat_marks SET resolved_at = NULL WHERE agent = ? AND session_id = ? AND resolved_at < ?`},
		{&b.dropEmptyMarks, `DELETE FROM state.chat_marks WHERE agent = ? AND session_id = ? AND resolved_at IS NULL AND pinned_at IS NULL AND parent_id IS NULL`},
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
	for _, st := range []*sql.Stmt{b.upsert, b.truncate, b.chainAt, b.markStale, b.prior, b.setTurn, b.setStatus,
		b.unresolve, b.dropEmptyMarks, b.releaseModel, b.resetModel} {
		if st != nil {
			st.Close()
		}
	}
	b.tx.Rollback()
}

// syncEntry brings one session's rows up to date, rewriting them only from
// the first that changed. listed is whether the chat was already cached;
// moved is whether its row reads differently: its first transcript, or a
// changed status or last turn.
func (b *batch) syncEntry(e Entry) (listed, moved bool, err error) {
	kind, id := string(e.Meta.Agent), e.Meta.ID
	// What we already had, so an append can be told from a rewrite.
	var priorSID int64
	var oldCount int
	// SQLite integers are signed; a uint64 hash round-trips through int64.
	var storedHash, syncedAt, storedUpdated, storedSize int64
	var turnKey, modelTurnKey string
	var turnStarted, turnFinished sql.NullInt64
	switch err := b.prior.QueryRow(kind, id).Scan(&priorSID, &oldCount, &storedHash, &syncedAt, &storedUpdated, &storedSize,
		&turnKey, &turnStarted, &turnFinished, &modelTurnKey); err {
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

	rows := eventRows(e.Events)
	keep := 0
	if listed && oldCount > 0 {
		if keep, err = b.kept(priorSID, rows, oldCount, storedHash); err != nil {
			return false, false, err
		}
	}
	// Growth over unchanged rows is an append the client keeps; a changed or
	// dropped row it holds is a rewrite it must reload. A row with no events
	// yet has no history to invalidate.
	bump := 0
	if keep < oldCount {
		bump = 1
		// A rewind can legitimately restore a model from an older row.
		if _, err := b.resetModel.Exec(kind, id); err != nil {
			return false, false, err
		}
	}

	// The same stamps a hook would have left, read off the file, so a chat
	// indexed cold times its last turn like one the host watched live.
	key, started, finished := mergeTurn(turnKey, turnStarted, turnFinished, transcriptTurn(e.Events))
	// Only a model-bearing row from this turn can replace its live model:
	// fresh output or a file timestamp may still accompany an older model.
	model, modelLabel := e.Meta.Model, e.Meta.ModelLabel
	var modelAt int64
	if !e.Meta.ModelAt.IsZero() {
		modelAt = e.Meta.ModelAt.UnixMilli()
	}
	held := modelTurnKey != "" && key == modelTurnKey &&
		(model == "" || e.Meta.ModelAt.IsZero() || !started.Valid || e.Meta.ModelAt.UnixMilli() < started.Int64)
	if held {
		model, modelLabel = "", ""
	}

	// Writes 2 and 3 in queries.go: the prompt reaching the file, then the
	// reply. RETURNING gives the surrogate key whether the row was new or not.
	var sid, searchHash int64
	var title string
	agentTitled := collapseSpace(e.Meta.Title) != ""
	if err := b.upsert.QueryRow(kind, id, e.Meta.Cwd, titleFor(e.Meta, e.Events), summaryOf(e.Events), e.Meta.Path,
		createdAt(e.Meta, e.Events), e.Meta.UpdatedAt.UnixMilli(), e.Meta.SizeBytes, len(e.Events), chainOf(rows, len(rows)), b.now,
		e.Meta.ContextUsed, e.Meta.ContextSize, model, modelLabel, modelAt, e.Meta.ProjectAgent,
		string(e.Meta.Settings.PermissionMode), e.Meta.Settings.Mode, e.Meta.Settings.ReasoningLevel, e.Meta.Settings.FastMode,
		lastMessageAt(e.Events), b.s.rev.Add(1), agentTitled, bump).Scan(&sid, &title, &searchHash); err != nil {
		return false, false, err
	}
	if modelTurnKey != "" && !held {
		if _, err := b.releaseModel.Exec(sid); err != nil {
			return false, false, err
		}
		moved = true
	}
	if _, err := b.truncate.Exec(sid, keep); err != nil {
		return false, false, err
	}
	if err := insertEvents(b.tx, sid, rows, keep); err != nil {
		return false, false, err
	}
	// Search is rebuilt from the whole chat. A live flush leaves that to the
	// next sweep rather than paying it every few hundred milliseconds.
	if e.DeferSearch {
		if _, err := b.markStale.Exec(sid); err != nil {
			return false, false, err
		}
	} else if err := indexChat(b.tx, sid, title, searchBody(e.Events), searchHash); err != nil {
		return false, false, err
	}
	if key != turnKey || started != turnStarted || finished != turnFinished {
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

// kept is how many of the oldCount stored rows rows leaves unchanged. Chains
// agree up to the first changed row and differ after it, so the answer is a
// binary search, and an append (the usual case) needs no lookup at all.
func (b *batch) kept(sid int64, rows []row, oldCount int, storedHash int64) (int, error) {
	n := min(oldCount, len(rows))
	if n == oldCount && chainOf(rows, n) == storedHash {
		return n, nil
	}
	// Invariant: the first lo rows match; past hi they do not.
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi + 1) / 2
		var stored int64
		switch err := b.chainAt.QueryRow(sid, mid-1).Scan(&stored); {
		case err == sql.ErrNoRows:
			hi = mid - 1
			continue
		case err != nil:
			return 0, err
		}
		if stored == rows[mid-1].chain {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo, nil
}

// readBefore reports whether e is an older read of the file than the stored
// one: an earlier time and no larger, a smaller file at the same time, or the
// same file read before its last rows landed. An earlier time on a larger file
// is a newer read: the file only grows, so the clock was set back, and trusting
// the time would drop every write until it caught up.
func readBefore(e Entry, storedUpdated, storedSize int64, storedCount int) bool {
	updated, size := e.Meta.UpdatedAt.UnixMilli(), e.Meta.SizeBytes
	switch {
	case updated < storedUpdated:
		return size <= storedSize
	case updated > storedUpdated:
		return false
	case size != storedSize:
		return size < storedSize
	}
	return len(e.Events) < storedCount
}

// transcriptStatus is the state the transcript records, and when. Only Codex
// writes a lifecycle; Claude reads unknown and takes hooks'. Working dates from
// the last line, so a chat settled as stale works again when its file grows.
func transcriptStatus(events []agent.Event) (agent.ChatStatus, int64) {
	status, at, last := agent.ChatUnknown, int64(0), int64(0)
	for _, e := range events {
		last = max(last, e.At)
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
	if status == agent.ChatWorking {
		at = last
	}
	return status, at
}

// insertEvents writes rows in multi-VALUES statements: each statement is one
// cgo crossing and one VM entry, the dominant per-row cost.
const eventsPerStatement = 128

// insertEvents writes a chat's rows from index from on.
func insertEvents(tx *sql.Tx, sid int64, rows []row, from int) error {
	return insertRows(tx, `INSERT INTO events (sid, idx, kind, turn, text, tool, at, chain) VALUES `, sid, rows, from, true)
}

// insertRows writes rows[from:] under owner with insert, the statement's head
// up to VALUES: a chat's into events, with its chain, and a subagent's into
// subagent_events, without.
func insertRows(tx *sql.Tx, insert string, owner int64, rows []row, from int, chained bool) error {
	cols, tuple := 7, `(?,?,?,?,?,?,?)`
	if chained {
		cols, tuple = 8, `(?,?,?,?,?,?,?,?)`
	}
	for start := from; start < len(rows); start += eventsPerStatement {
		end := min(start+eventsPerStatement, len(rows))
		n := end - start

		var b strings.Builder
		b.WriteString(insert)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(tuple)
		}

		args := make([]any, 0, n*cols)
		for i := start; i < end; i++ {
			r := rows[i]
			args = append(args, owner, i, r.kind, r.turn, r.text, r.tool, r.at)
			if chained {
				args = append(args, r.chain)
			}
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
		m := Message{Index: i, Kind: string(e.Kind), Turn: e.TurnID, At: e.At}
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
	return s.SyncBatch([]Entry{{Meta: meta, Events: events}})
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
