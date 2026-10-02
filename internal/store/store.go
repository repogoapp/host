// Package store is the host's SQLite: cache.db, normalized sessions rebuilt
// from transcripts (never the source of truth), and state.db, what the user
// made, which no transcript holds. A session is replaced wholesale
// with a bumped `generation`, and rows are keyed by position because parser
// ids regenerate on rebuild.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/sqlitedb"
)

type Store struct {
	db *sql.DB

	// The sync surface, seeded once at Open. See sync.go.
	epoch  string
	rev    atomic.Int64
	syncMu sync.Mutex

	// Told what each commit did to the chat list; see notify.go.
	onChange func(Change)
}

// schemaVersion fingerprints schema in PRAGMA user_version, so a cache built by
// any other schema is deleted and reparsed rather than migrated.
var schemaVersion = func() int32 {
	h := fnv.New32a()
	h.Write([]byte(schema))
	return int32(h.Sum32()&0x7fffffff) | 1
}()

// The files Open keeps in its directory, one per lifetime: the cache is
// deleted and rebuilt when its schema changes; state is never deleted.
const (
	CacheFile = "cache.db"
	StateFile = "state.db"
)

// Open opens state.db and cache.db in dir, with state attached to the cache's
// connection as `state`. synchronous=NORMAL for the cache because FULL let an
// initial sync dirty gigabytes and get the process killed on a disk-write limit.
func Open(dir string) (*Store, error) {
	statePath := filepath.Join(dir, StateFile)
	if err := openState(statePath); err != nil {
		return nil, fmt.Errorf("store: open state: %w", err)
	}
	db, err := openCurrent(filepath.Join(dir, CacheFile), statePath)
	if err != nil {
		return nil, err
	}
	// A cached active state is not proof that the agent survived the host restart.
	settled := append(slices.Clone(agent.ActiveStatuses), agent.ChatIdle)
	if _, err := db.Exec(`UPDATE sessions SET status = ? WHERE status IN (`+marks(len(settled))+`)`,
		append([]any{agent.ChatUnknown}, anys(settled)...)...); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.openSync(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// openCurrent opens the cache at path, replacing a file whose schema is not
// this one's. Only the cache's own files go; state.db is attached, not owned.
func openCurrent(path, statePath string) (*sql.DB, error) {
	db := connect(path, statePath)
	var version int32
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version == schemaVersion {
		return db, nil
	}
	db.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	db = connect(path, statePath)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// connect opens the cache with state attached, in the connect hook so a
// replaced connection has it too.
func connect(path, statePath string) *sql.DB {
	db := sqlitedb.Open(path, append([]string{
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA temp_store=MEMORY",
		// Negative is KiB of page cache rather than a page count: 64MB.
		"PRAGMA cache_size=-64000",
	}, attachState(statePath)...)...)
	// SQLite has a single writer; a second connection only invites SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	return db
}

// marks is n comma-separated SQL placeholders.
func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func anys[T any](values []T) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func (s *Store) Close() error { return s.db.Close() }

// Entry is one session and its normalized events.
type Entry struct {
	Meta   session.Meta
	Events []agent.Event
}

// Fingerprint is what a session looked like at its last sync.
type Fingerprint struct {
	UpdatedAt int64
	SizeBytes int64
}

// Fingerprints loads every synced session's freshness marker in one query, so a
// resync can skip unchanged files without a round trip each.
func (s *Store) Fingerprints() (map[string]Fingerprint, error) {
	rows, err := s.db.Query(`SELECT agent, session_id, updated_at, size_bytes FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]Fingerprint{}
	for rows.Next() {
		var kind, id string
		var fp Fingerprint
		if err := rows.Scan(&kind, &id, &fp.UpdatedAt, &fp.SizeBytes); err != nil {
			return nil, err
		}
		out[key(kind, id)] = fp
	}
	return out, rows.Err()
}

// Unchanged reports whether a session on disk matches what was last synced.
func Unchanged(fp Fingerprint, meta session.Meta) bool {
	return fp.SizeBytes == meta.SizeBytes && fp.UpdatedAt == meta.UpdatedAt.UnixMilli()
}

// Key names a session in Fingerprints and Prune.
func Key(meta session.Meta) string { return key(string(meta.Agent), meta.ID) }

func key(kind, id string) string { return kind + "\x00" + id }

// Counts returns row totals, for reporting.
func (s *Store) Counts() (sessions, events int64, err error) {
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		return
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events)
	return
}
