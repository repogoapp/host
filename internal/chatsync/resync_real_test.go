package chatsync

import (
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agents/claude"
	"github.com/repogoapp/host/internal/agents/codex"
	"github.com/repogoapp/host/internal/session"
	"github.com/repogoapp/host/internal/sqlitedb"
	"github.com/repogoapp/host/internal/store"
)

var (
	realTranscripts = flag.Bool("real-transcripts", false, "compare incremental resyncs with full rewrites on local transcripts")
	realCount       = flag.Int("real-count", 3, "how many of each agent's largest sessions -real-transcripts reads")
)

// TestResyncMatchesFullRewrite replays each large local session as its file
// grew, syncing every stage into one cache as a live flush does, and checks it
// against a cache that synced the whole file at once: every row and column,
// the chat's row and its search text. A rewind and a mid-history edit follow.
// Run: go test ./internal/chatsync -run TestResyncMatchesFullRewrite -real-transcripts -v
func TestResyncMatchesFullRewrite(t *testing.T) {
	if !*realTranscripts {
		t.Skip("pass -real-transcripts to read local chat history")
	}
	providers := []session.Provider{
		claude.New(agent.Dependencies{}).Sessions(),
		codex.New(agent.Dependencies{}).Sessions(),
	}
	for _, p := range providers {
		metas, err := p.List()
		if err != nil {
			t.Fatalf("%s: list: %v", p.Kind(), err)
		}
		slices.SortFunc(metas, func(a, b session.Meta) int { return int(b.SizeBytes - a.SizeBytes) })
		for _, meta := range metas[:min(*realCount, len(metas))] {
			t.Run(fmt.Sprintf("%s/%s", p.Kind(), meta.ID), func(t *testing.T) {
				replay(t, p, meta)
			})
		}
	}
}

func replay(t *testing.T, p session.Provider, meta session.Meta) {
	stages := growth(t, p, meta)
	final := stages[len(stages)-1]
	t.Logf("%.0f MB, %d events, %d stages", float64(meta.SizeBytes)/1e6, len(final.Events), len(stages))

	inc := openStore(t)
	var last time.Duration
	for i, stage := range stages {
		// Every other stage defers search, as a live flush does; the rest are sweeps.
		stage.DeferSearch = i%2 == 1
		started := time.Now()
		if err := inc.SyncBatch([]store.Entry{stage}); err != nil {
			t.Fatalf("stage %d: %v", i, err)
		}
		last = time.Since(started)
	}
	indexStale(t, inc)
	appendGen := generation(t, inc, meta)

	full := openStore(t)
	started := time.Now()
	if err := full.SyncBatch([]store.Entry{final}); err != nil {
		t.Fatal(err)
	}
	t.Logf("last append %v, full write %v; %d of %d stages rewrote held rows",
		last, time.Since(started), appendGen-1, len(stages)-1)
	compare(t, "growth", inc, full, meta)

	// A rewind: the file goes back to an earlier stage, then forward again.
	rewound := stages[len(stages)/2]
	rewound.Meta.UpdatedAt = final.Meta.UpdatedAt.Add(time.Second)
	syncOne(t, inc, rewound)
	again := final
	again.Meta.UpdatedAt = rewound.Meta.UpdatedAt.Add(time.Second)
	syncOne(t, inc, again)
	if g := generation(t, inc, meta); g <= appendGen {
		t.Errorf("rewind kept generation %d; a device would keep rows that went", g)
	}
	compare(t, "rewind", inc, full, meta)

	// An edit mid-history, as Codex re-segmenting a rollout can make.
	edited := again
	edited.Events = slices.Clone(final.Events)
	mid := len(edited.Events) / 2
	edited.Events[mid].Text += " (edited)"
	edited.Meta.UpdatedAt = again.Meta.UpdatedAt.Add(time.Second)
	syncOne(t, inc, edited)
	fullEdited := openStore(t)
	syncOne(t, fullEdited, edited)
	compare(t, "edit", inc, fullEdited, meta)
}

// growth is meta's session at about a dozen points of its life: the newest
// file cut at line boundaries, the files before it whole, each stage parsed
// as the provider parses the real thing.
func growth(t *testing.T, p session.Provider, meta session.Meta) []store.Entry {
	files := meta.Files()
	newest, err := os.ReadFile(files[len(files)-1])
	if err != nil {
		t.Fatal(err)
	}
	var ends []int
	for i, b := range newest {
		if b == '\n' {
			ends = append(ends, i+1)
		}
	}
	if len(ends) == 0 {
		t.Skip("empty transcript")
	}
	// Ten even cuts, then the last five lines one at a time: a live turn's appends.
	var cuts []int
	for i := 1; i <= 10; i++ {
		cuts = append(cuts, ends[(len(ends)-1)*i/10])
	}
	for i := max(len(ends)-5, 0); i < len(ends); i++ {
		cuts = append(cuts, ends[i])
	}
	slices.Sort(cuts)
	cuts = slices.Compact(cuts)

	dir := t.TempDir()
	var before int64
	stagedFiles := make([]string, len(files))
	for i, f := range files[:len(files)-1] {
		stagedFiles[i] = f
		if st, err := os.Stat(f); err == nil {
			before += st.Size()
		}
	}
	var stages []store.Entry
	for i, cut := range cuts {
		path := filepath.Join(dir, fmt.Sprintf("stage-%02d-%s", i, filepath.Base(files[len(files)-1])))
		if err := os.WriteFile(path, newest[:cut], 0o600); err != nil {
			t.Fatal(err)
		}
		stagedFiles[len(files)-1] = path
		m := meta
		m.Path, m.Paths = path, nil
		if len(files) > 1 {
			m.Paths = slices.Clone(stagedFiles)
		}
		m.SizeBytes = before + int64(cut)
		m.UpdatedAt = meta.UpdatedAt.Add(time.Duration(i-len(cuts)) * time.Second)
		events, err := session.ReadAll(m, p)
		if err != nil {
			t.Fatal(err)
		}
		// The stored row names the real file, whichever stage wrote it.
		m.Path, m.Paths = meta.Path, meta.Paths
		stages = append(stages, store.Entry{Meta: m, Events: events})
	}
	return stages
}

// cache is a store and the folder it is in, for reading its file directly.
type cache struct {
	*store.Store
	dir string
}

func openStore(t *testing.T) cache {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return cache{s, dir}
}

func syncOne(t *testing.T, s cache, e store.Entry) {
	t.Helper()
	if err := s.SyncBatch([]store.Entry{e}); err != nil {
		t.Fatal(err)
	}
}

func indexStale(t *testing.T, s cache) {
	t.Helper()
	if _, err := s.IndexStale(); err != nil {
		t.Fatal(err)
	}
}

// read opens a store's cache on a connection of its own, for comparing.
func read(t *testing.T, s cache) *sql.DB {
	t.Helper()
	db := sqlitedb.Open(filepath.Join(s.dir, store.CacheFile))
	t.Cleanup(func() { db.Close() })
	return db
}

func generation(t *testing.T, s cache, meta session.Meta) int {
	t.Helper()
	var g int
	if err := read(t, s).QueryRow(`SELECT generation FROM sessions WHERE agent = ? AND session_id = ?`,
		string(meta.Agent), meta.ID).Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

// compare fails unless a and b hold meta's chat alike: its rows, column for
// column, the sessions fields a sync writes, and its search row.
func compare(t *testing.T, step string, a, b cache, meta session.Meta) {
	t.Helper()
	dump := func(s cache) (string, [][]byte) {
		db := read(t, s)
		var head string
		var sid int64
		if err := db.QueryRow(`SELECT sid, printf('%d|%d|%d|%s|%s', event_count, content_hash, size_bytes, title, summary)
			FROM sessions WHERE agent = ? AND session_id = ?`, string(meta.Agent), meta.ID).Scan(&sid, &head); err != nil {
			t.Fatal(err)
		}
		var search sql.NullString
		if err := db.QueryRow(`SELECT title || char(0) || body FROM chat_search WHERE rowid = ?`, sid).Scan(&search); err != nil &&
			err != sql.ErrNoRows {
			t.Fatal(err)
		}
		head += "|search:" + search.String
		rows, err := db.Query(`SELECT idx, kind, quote(turn), quote(text), quote(tool), quote(at), chain FROM events WHERE sid = ? ORDER BY idx`, sid)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out [][]byte
		for rows.Next() {
			var idx, chain int64
			var kind, turn, text, tool, at string
			if err := rows.Scan(&idx, &kind, &turn, &text, &tool, &at, &chain); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Appendf(nil, "%d|%s|%s|%s|%s|%s|%d", idx, kind, turn, text, tool, at, chain))
		}
		return head, out
	}
	headA, rowsA := dump(a)
	headB, rowsB := dump(b)
	if headA != headB {
		t.Errorf("%s: chat rows differ:\n incremental %.300q\n full        %.300q", step, headA, headB)
	}
	if len(rowsA) != len(rowsB) {
		t.Errorf("%s: %d rows incremental, %d full", step, len(rowsA), len(rowsB))
	}
	for i := range min(len(rowsA), len(rowsB)) {
		if !bytes.Equal(rowsA[i], rowsB[i]) {
			t.Errorf("%s: row %d differs:\n incremental %.300q\n full        %.300q", step, i, rowsA[i], rowsB[i])
			return
		}
	}
}
