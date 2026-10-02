package shipping

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/repogo/host/internal/par"
	"github.com/repogo/host/internal/sqlitedb"
)

const (
	// A background sweep keeps the ledger near current without a phone asking.
	sweepEvery = 5 * time.Minute
	startDelay = 30 * time.Second
	// An answer is at most this stale; older, it sweeps first.
	freshFor = time.Minute
	// Files read at once, and files written per transaction.
	parseParallel = 4
	writeBatch    = 64
	// lineLimit matches the session parser's: longer lines are skipped.
	lineLimit = 16 << 20
	// ledgerVersion is bumped whenever a parser rule changes what a file
	// yields; every file is then read again and its rows corrected in place.
	ledgerVersion = 1
)

// Ledger is the usage database: one row per request, kept after its
// transcript is gone, so a year of history survives the agents' own cleanup.
// It is not the disposable chat cache; its rows are not re-derivable.
type Ledger struct {
	db        *sql.DB
	providers []Provider
	pricing   *pricing

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	sweeping chan struct{}
	sweptAt  time.Time
}

// Open opens or creates the ledger at path; rateCache is where public model
// rates are kept between fetches. Sweeps run on ctx, not on the caller that
// asked for one, so a phone giving up does not stop the index.
func Open(ctx context.Context, path, rateCache string, providers ...Provider) (*Ledger, error) {
	db := sqlitedb.Open(path, `PRAGMA synchronous=NORMAL`, `PRAGMA busy_timeout=5000`)
	db.SetMaxOpenConns(1)
	if err := create(db); err != nil {
		db.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Ledger{
		db: db, providers: providers,
		pricing: &pricing{path: rateCache, fetch: download},
		ctx:     ctx, cancel: cancel,
	}, nil
}

func create(db *sql.DB) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS requests (
		  agent TEXT NOT NULL, key TEXT NOT NULL,
		  session TEXT NOT NULL, at INTEGER NOT NULL, model TEXT NOT NULL,
		  input INTEGER NOT NULL, cache_read INTEGER NOT NULL,
		  cache_write_5m INTEGER NOT NULL, cache_write_1h INTEGER NOT NULL,
		  output INTEGER NOT NULL, reasoning INTEGER NOT NULL,
		  fast INTEGER NOT NULL DEFAULT 0,
		  reported_micros INTEGER,
		  path TEXT NOT NULL,
		  PRIMARY KEY (agent, key)
		) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS requests_at ON requests(at)`,
		// What each file was when last read, so an unchanged one is skipped.
		`CREATE TABLE IF NOT EXISTS files (
		  path TEXT PRIMARY KEY, size INTEGER NOT NULL, mtime_ms INTEGER NOT NULL
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID`,
	} {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == ledgerVersion {
		return nil
	}
	// Rows stay: a file read again rewrites its own, and a gone file's are history.
	if _, err := db.Exec(`DELETE FROM files`); err != nil {
		return err
	}
	_, err := db.Exec(`PRAGMA user_version = ` + strconv.Itoa(ledgerVersion))
	return err
}

// Close stops a sweep in progress and closes the database.
func (l *Ledger) Close() error {
	l.cancel()
	l.mu.Lock()
	done := l.sweeping
	l.mu.Unlock()
	if done != nil {
		<-done
	}
	return l.db.Close()
}

// Run indexes once the host has settled, then keeps the ledger current until
// ctx ends. The delay leaves the first chat sync the disk to itself; a phone
// asking sooner starts the index at once.
func (l *Ledger) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(startDelay):
	}
	l.refresh(ctx, 0)
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.refresh(ctx, sweepEvery/2)
		}
	}
}

// refresh sweeps unless one finished within maxAge, joining a sweep already
// running. It waits until the sweep ends or ctx does; the sweep itself runs
// on the ledger's own context, so a caller giving up does not stop it.
func (l *Ledger) refresh(ctx context.Context, maxAge time.Duration) {
	l.mu.Lock()
	if !l.sweptAt.IsZero() && time.Since(l.sweptAt) < maxAge {
		l.mu.Unlock()
		return
	}
	done := l.sweeping
	if done == nil {
		done = make(chan struct{})
		l.sweeping = done
		go func() {
			err := l.sweep(l.ctx)
			l.mu.Lock()
			if err == nil {
				l.sweptAt = time.Now()
			}
			l.sweeping = nil
			l.mu.Unlock()
			close(done)
		}()
	}
	l.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// indexed reports whether a sweep has ever finished, so the ledger holds all
// history the transcripts had and not just the files read so far.
func (l *Ledger) indexed() bool {
	var v string
	return l.db.QueryRow(`SELECT value FROM meta WHERE key = 'indexed'`).Scan(&v) == nil
}

// sweep reads every transcript that changed since it was last read and writes
// its requests. A file that disappeared keeps its rows.
func (l *Ledger) sweep(ctx context.Context) error {
	known := map[string][2]int64{}
	rows, err := l.db.QueryContext(ctx, `SELECT path, size, mtime_ms FROM files`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var path string
		var size, mtime int64
		if err := rows.Scan(&path, &size, &mtime); err != nil {
			rows.Close()
			return err
		}
		known[path] = [2]int64{size, mtime}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var changed []transcript
	for _, p := range l.providers {
		for _, root := range p.UsageRoots() {
			for _, f := range discover(p, root) {
				if known[f.path] != [2]int64{f.size, f.mtimeMs} {
					changed = append(changed, f)
				}
			}
		}
	}
	for start := 0; start < len(changed); start += writeBatch {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := changed[start:min(start+writeBatch, len(changed))]
		type parsed struct {
			records []Record
			err     error
		}
		results := par.Map(chunk, parseParallel, func(f transcript) parsed {
			records, err := parseFile(f)
			return parsed{records, err}
		})
		if err := l.write(ctx, chunk, func(i int) ([]Record, error) { return results[i].records, results[i].err }); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_, err = l.db.Exec(`INSERT OR IGNORE INTO meta (key, value) VALUES ('indexed', '1')`)
	return err
}

// write stores a chunk's requests and fingerprints in one transaction. An
// unreadable file keeps its old fingerprint, so the next sweep tries again.
func (l *Ledger) write(ctx context.Context, files []transcript, result func(int) ([]Record, error)) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The file a row came from rewrites it whole when read again, so a parser
	// fix corrects rows in place. A copy in another file (a resume or fork)
	// only replaces a shorter reading of the request, keeping the owner.
	insert, err := tx.Prepare(`INSERT INTO requests
		(agent, key, session, at, model, input, cache_read, cache_write_5m, cache_write_1h, output, reasoning, fast, reported_micros, path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent, key) DO UPDATE SET
		  session = CASE WHEN requests.path = excluded.path THEN excluded.session ELSE requests.session END,
		  at = CASE WHEN requests.path = excluded.path THEN excluded.at ELSE requests.at END,
		  model = excluded.model, input = excluded.input, cache_read = excluded.cache_read,
		  cache_write_5m = excluded.cache_write_5m, cache_write_1h = excluded.cache_write_1h,
		  output = excluded.output, reasoning = excluded.reasoning, fast = excluded.fast,
		  reported_micros = excluded.reported_micros
		WHERE requests.path = excluded.path OR excluded.output > requests.output`)
	if err != nil {
		return err
	}
	defer insert.Close()
	mark, err := tx.Prepare(`INSERT INTO files (path, size, mtime_ms) VALUES (?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET size = excluded.size, mtime_ms = excluded.mtime_ms`)
	if err != nil {
		return err
	}
	defer mark.Close()

	for i, f := range files {
		records, err := result(i)
		if err != nil {
			continue
		}
		kind := string(f.source.Kind())
		for key, r := range longest(f.path, records) {
			var reported any
			if r.HasReported {
				reported = micros(r.ReportedUSD)
			}
			t := r.Tokens
			if _, err := insert.Exec(kind, key, r.Session, r.TimestampMs, r.Model,
				t.Uncached, t.Cached, t.Creation, t.Creation1h, t.Output, t.Reasoning, r.Fast, reported, f.path); err != nil {
				return err
			}
		}
		if _, err := mark.Exec(f.path, f.size, f.mtimeMs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// longest keys a file's records, a request written on several lines (a
// streamed reply) once, as its longest reading; a request with no key of its
// own is keyed by file and position.
func longest(path string, records []Record) map[string]Record {
	out := make(map[string]Record, len(records))
	for n, r := range records {
		key := r.Key
		if key == "" {
			key = path + "#" + strconv.Itoa(n)
		}
		if seen, ok := out[key]; !ok || r.Tokens.Output > seen.Tokens.Output {
			out[key] = r
		}
	}
	return out
}

// parseFile reads one transcript's requests: the Rust parser when linked,
// else the provider's line parser. Only complete lines count; a trailing
// fragment is a write in progress, read whole by a later sweep.
func parseFile(f transcript) ([]Record, error) {
	if native, ok := f.source.(FileParser); ok {
		if records, ok := native.ParseUsageFile(f.path); ok {
			return records, nil
		}
	}
	handle, err := os.Open(f.path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()

	var records []Record
	parse := f.source.NewUsageParser()
	reader := bufio.NewReaderSize(handle, 1<<20)
	var long []byte
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(long) <= lineLimit {
				long = append(long, line...)
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return records, nil
			}
			return nil, err
		}
		if len(long) > 0 {
			line = append(long, line...)
			long = long[:0]
		}
		if len(line) > lineLimit {
			continue
		}
		if rec := parse(line); rec != nil {
			records = append(records, *rec)
		}
	}
}
