package store

import (
	"database/sql"
	"fmt"
	"hash/fnv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/repogo/host/internal/agent"
)

// searchBody is the text a chat is found by: the user's prompts and the
// agent's replies, in order. Tool output and reasoning are left out.
func searchBody(events []agent.Event) string {
	var b strings.Builder
	for _, e := range events {
		if e.Text == "" || (e.Kind != agent.EventUserMessage && e.Kind != agent.EventText) {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(e.Text)
	}
	return b.String()
}

// searchHash fingerprints a chat's search row. Never zero, so a new session
// row (search_hash 0) is always indexed.
func searchHash(title, body string) int64 {
	h := fnv.New64a()
	h.Write([]byte(title))
	h.Write([]byte{0})
	h.Write([]byte(body))
	return int64(h.Sum64() | 1)
}

// typographic is punctuation FTS4's porter tokenizer would glue into a word
// ("“quoted”", "foo—bar"), mapped to the ASCII it treats as a separator.
var typographic = map[rune]string{
	'‘': "'", '’': "'", '‚': "'", '‛': "'", '′': "'",
	'“': `"`, '”': `"`, '„': `"`, '‟': `"`, '″': `"`,
	'‐': "-", '‑': "-", '‒': "-", '–': "-", '—': "-", '―': "-", '−': "-",
	'…': "...",
}

// indexText is text as the index holds it. The tokenizer splits only on ASCII
// punctuation and folds only ASCII case, so other punctuation, symbols and
// spaces become separators and other letters lowercase, as a query's do.
func indexText(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case typographic[r] != "":
			b.WriteString(typographic[r])
		case unicode.IsLetter(r):
			b.WriteRune(unicode.ToLower(r))
		case unicode.IsNumber(r) || unicode.IsMark(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// indexChat replaces the chat's chat_search row inside the sync transaction,
// so the index and the chat cannot disagree. A sync that changed neither the
// title nor the text leaves the row alone.
func indexChat(tx *sql.Tx, sid int64, title string, events []agent.Event, storedHash int64) error {
	title, body := indexText(title), indexText(searchBody(events))
	hash := searchHash(title, body)
	if hash == storedHash {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM chat_search WHERE rowid = ?`, sid); err != nil {
		return fmt.Errorf("store: index chat: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO chat_search (rowid, title, body) VALUES (?, ?, ?)`, sid, title, body); err != nil {
		return fmt.Errorf("store: index chat: %w", err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET search_hash = ? WHERE sid = ?`, hash, sid); err != nil {
		return fmt.Errorf("store: index chat: %w", err)
	}
	return nil
}

// deleteSearchRow drops a chat's chat_search row by chat id. It must run
// before the chat's sessions row goes, since that row maps the id to the rowid.
const deleteSearchRow = `DELETE FROM chat_search WHERE rowid IN (SELECT sid FROM sessions WHERE agent = ? AND session_id = ?)`

// maxSearchChars caps what a query is built from; a longer one is a paste,
// not a search.
const maxSearchChars = 200

// fillerWords are dropped from a query: they are in nearly every chat, so they
// narrow nothing and make the match slow. Spoken queries carry most of them.
var fillerWords = map[string]bool{
	"a": true, "about": true, "an": true, "and": true, "are": true, "can": true,
	"chat": true, "chats": true, "could": true, "did": true, "do": true, "does": true,
	"find": true, "for": true, "from": true, "had": true, "has": true, "have": true,
	"hey": true, "how": true, "i": true, "in": true, "is": true, "it": true,
	"me": true, "my": true, "of": true, "on": true, "or": true, "please": true,
	"show": true, "so": true, "tell": true, "that": true, "the": true, "them": true,
	"this": true, "to": true, "was": true, "we": true, "were": true, "what": true,
	"when": true, "where": true, "which": true, "who": true, "with": true,
	"you": true, "your": true,
}

// searchWords splits a query where the index's tokenizer does (anything but a
// letter or digit), so "tool-call" is "tool" and "call" as the index saw it,
// and drops filler words unless that would leave nothing.
func searchWords(query string) []string {
	if runes := []rune(query); len(runes) > maxSearchChars {
		query = string(runes[:maxSearchChars])
	}
	all := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	})
	var kept []string
	for _, w := range all {
		if !fillerWords[w] {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		return all
	}
	return kept
}

// minPrefixLetters is the shortest last word matched as a prefix: "w*" expands
// to thousands of words and takes a tenth of a second.
const minPrefixLetters = 3

// searchMatch is the FTS4 query: every word in the chat, the last a prefix so a
// word still being typed matches. Words are lowercase letters and digits and
// FTS4's operators uppercase, so nothing typed is syntax. Empty with no words.
func searchMatch(query string) string {
	words := searchWords(query)
	if len(words) == 0 {
		return ""
	}
	match := strings.Join(words, " ")
	if utf8.RuneCountInString(words[len(words)-1]) >= minPrefixLetters {
		match += "*"
	}
	return match
}

// Snippet markers around each matched word, as the phone's highlight expects.
const (
	matchOpen  = "⟪"
	matchClose = "⟫"
)

// addMatches sets each chat's Match: the words around the hit in its prompts
// and replies. A chat found by its title alone keeps an empty Match.
func (s *Store) addMatches(chats []Chat, match string) error {
	if len(chats) == 0 {
		return nil
	}
	values := make([]string, len(chats))
	args := []any{match}
	byID := make(map[ChatID]*Chat, len(chats))
	for i := range chats {
		agent, sessionID, err := chats[i].ID.parts()
		if err != nil {
			return err
		}
		values[i] = "(?, ?)"
		args = append(args, agent, sessionID)
		byID[chats[i].ID] = &chats[i]
	}
	rows, err := s.db.Query(`SELECT s.agent, s.session_id, snippet(chat_search, ?, ?, '…', 1, 16)
		FROM chat_search JOIN sessions s ON s.sid = chat_search.rowid
		WHERE chat_search MATCH ? AND (s.agent, s.session_id) IN (VALUES `+strings.Join(values, ", ")+`)`,
		append([]any{matchOpen, matchClose}, args...)...)
	if err != nil {
		return fmt.Errorf("store: search snippets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var agent, sessionID, snippet string
		if err := rows.Scan(&agent, &sessionID, &snippet); err != nil {
			return err
		}
		chat := byID[chatID(agent, sessionID)]
		if chat == nil || !strings.Contains(snippet, matchOpen) {
			continue
		}
		chat.Match = strings.Join(strings.Fields(snippet), " ")
	}
	return rows.Err()
}
