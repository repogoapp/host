// Command search measures chats.list's search with the host's own
// store code: how fast a search is, what the index costs on disk, and a delete
// by chat id.
//
// It reads the database `go run ./bench` writes from the real sessions, so
// run that first; the full reparse it times is what the index adds to a sync:
//
//	go run ./bench && go run ./bench/search
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/repogo/host/internal/store"
)

func main() {
	var (
		dbDir = flag.String("dir", "/tmp/repogo-bench", "directory `go run ./bench` wrote; the delete changes it")
		limit = flag.Int("limit", 50, "chats per search, as the search bar asks")
		runs  = flag.Int("runs", 20, "timed runs per query")
	)
	flag.Parse()

	db, err := store.Open(*dbDir)
	check(err)
	defer db.Close()
	sessions, _, err := db.Counts()
	check(err)
	fmt.Printf("%d chats in %s\n\n", sessions, *dbDir)

	// 1. What searches find, with the words around the hit.
	fmt.Println("== results")
	for _, q := range []string{
		"can you find the chat about editing the queue for tool call queue actions",
		"voice agent restructuring tool calls",
		"sparkle", "fts5", "tool-call queue", "live activ",
		`"; DROP TABLE x; NEAR( *`,
	} {
		chats := search(db, q, *limit)
		fmt.Printf("%q  %d chats\n", q, len(chats))
		for _, c := range chats[:min(3, len(chats))] {
			fmt.Printf("    %-40.40s %.110s\n", c.Title, c.Match)
		}
	}
	fmt.Println()

	// 2. Speed: each query once cold, then `runs` times.
	fmt.Println("== speed")
	queries := []string{
		"can you find the chat about editing the queue for tool call queue actions",
		"voice agent restructuring tool calls",
		"find the chat about the app clip size",
		"live activity", "App Clip size", "push notifications", "sparkle release",
		"vercel sandbox", "relay encryption", "pairing code", "git store",
		"markdown table", "terminal", "browser tab", "queue", "the",
		"WorkspaceChangesStore", "fts5", "stripe", "zzzz nothing matches",
	}
	var all []time.Duration
	var cold time.Duration
	for _, q := range queries {
		t0 := time.Now()
		search(db, q, *limit)
		cold = max(cold, time.Since(t0))
		var own []time.Duration
		for range *runs {
			t0 := time.Now()
			search(db, q, *limit)
			own = append(own, time.Since(t0))
		}
		all = append(all, own...)
		slices.Sort(own)
		fmt.Printf("%-50.50q median %-9s worst %s\n", q, round(own[len(own)/2]), round(own[len(own)-1]))
	}
	slices.Sort(all)
	fmt.Printf("\nall %d searches  median %s  p95 %s  worst %s  (slowest first run %s)\n",
		len(all), round(all[len(all)/2]), round(all[len(all)*95/100]), round(all[len(all)-1]), round(cold))

	// Typing into the search bar: every keystroke from two characters on.
	typed := "live activity widget"
	var keys []time.Duration
	for n := 2; n <= len(typed); n++ {
		t0 := time.Now()
		search(db, typed[:n], *limit)
		keys = append(keys, time.Since(t0))
	}
	slices.Sort(keys)
	fmt.Printf("typing %q  %d keystrokes  median %s  worst %s\n\n",
		typed, len(keys), round(keys[len(keys)/2]), round(keys[len(keys)-1]))

	// 3. Size: the index's own tables, read beside the store.
	fmt.Println("== size")
	raw, err := sql.Open("sqlite3", "file:"+filepath.Join(*dbDir, store.CacheFile)+"?mode=ro")
	check(err)
	defer raw.Close()
	var dbBytes int64
	check(raw.QueryRow(`SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()`).Scan(&dbBytes))
	var words, text, lengths int64
	check(raw.QueryRow(`SELECT SUM(LENGTH(block)) FROM chat_search_segments`).Scan(&words))
	check(raw.QueryRow(`SELECT SUM(LENGTH(c0title) + LENGTH(c1body)) FROM chat_search_content`).Scan(&text))
	check(raw.QueryRow(`SELECT SUM(LENGTH(size)) FROM chat_search_docsize`).Scan(&lengths))
	fmt.Printf("cache.db     %.1f MB\n", mb(dbBytes))
	fmt.Printf("index words  %.1f MB\n", mb(words))
	fmt.Printf("index text   %.1f MB\n", mb(text))
	fmt.Printf("lengths      %.1f MB\n\n", mb(lengths))

	// 4. Delete by chat id: the first hit for a word goes, and the search no
	// longer finds it.
	fmt.Println("== delete by chat id")
	hits := search(db, "sparkle", *limit)
	if len(hits) == 0 {
		fmt.Println("no chat mentions sparkle; skipped")
		return
	}
	victim := hits[0].ID
	t0 := time.Now()
	check(db.Delete(victim))
	took := time.Since(t0)
	still := slices.ContainsFunc(search(db, "sparkle", *limit), func(c store.Chat) bool { return c.ID == victim })
	fmt.Printf("%s  %s  still found: %v\n", victim, round(took), still)
}

func search(db *store.Store, q string, limit int) []store.Chat {
	page, err := db.Chats(store.ChatQuery{Search: q, Limit: limit})
	check(err)
	return page.Chats
}

func mb(n int64) float64 { return float64(n) / (1 << 20) }

func round(d time.Duration) time.Duration {
	if d < time.Millisecond {
		return d.Round(time.Microsecond)
	}
	return d.Round(time.Millisecond)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "search bench:", err)
		os.Exit(1)
	}
}
