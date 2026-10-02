package shipping

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
)

// fakeProvider bills one request per line, `key session output fast`.
type fakeProvider struct{ root string }

func (p fakeProvider) Kind() agent.Kind     { return "fake" }
func (p fakeProvider) UsageRoots() []string { return []string{p.root} }
func (p fakeProvider) NewUsageParser() func([]byte) *Record {
	return func(line []byte) *Record {
		var key, session string
		var output int64
		var fast bool
		if _, err := fmt.Sscan(string(line), &key, &session, &output, &fast); err != nil {
			return nil
		}
		return &Record{Key: key, Session: session, TimestampMs: 1_000, Model: "m", Tokens: Tokens{Uncached: 10, Output: output}, Fast: fast}
	}
}

func openTestLedger(t *testing.T, root string, r rates) *Ledger {
	t.Helper()
	l, err := Open(t.Context(), filepath.Join(t.TempDir(), "usage.db"), filepath.Join(t.TempDir(), "rates.json"), fakeProvider{root})
	if err != nil {
		t.Fatal(err)
	}
	l.pricing.fetch = func(ctx context.Context) rates { return r }
	t.Cleanup(func() { l.Close() })
	return l
}

type row struct {
	session, path string
	output        int64
}

func rowOf(t *testing.T, l *Ledger, key string) row {
	t.Helper()
	var r row
	if err := l.db.QueryRow(`SELECT session, path, output FROM requests WHERE key = ?`, key).Scan(&r.session, &r.path, &r.output); err != nil {
		t.Fatal(err)
	}
	return r
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A parser fix re-reads every file; the file a row came from rewrites it, and
// a copy of the request in another file never takes it over.
func TestLedgerRereadCorrectsRowsInPlace(t *testing.T) {
	root := t.TempDir()
	original, copy := filepath.Join(root, "a.jsonl"), filepath.Join(root, "b.jsonl")
	write(t, original, "k1 wrong 5 false\n")
	write(t, copy, "k1 resumed 5 false\n")
	l := openTestLedger(t, root, rates{})
	l.refresh(t.Context(), 0)
	if got := rowOf(t, l, "k1"); got != (row{"wrong", original, 5}) {
		t.Fatalf("first read: %+v", got)
	}

	// What a ledgerVersion bump does: forget every fingerprint.
	write(t, original, "k1 right 5 false\n")
	if _, err := l.db.Exec(`DELETE FROM files`); err != nil {
		t.Fatal(err)
	}
	l.refresh(t.Context(), 0)
	if got := rowOf(t, l, "k1"); got != (row{"right", original, 5}) {
		t.Fatalf("after re-read: %+v", got)
	}

	// A longer reading in the copy fills the gap but keeps the owner and its chat.
	write(t, copy, "k1 resumed 9 false\n")
	l.refresh(t.Context(), 0)
	if got := rowOf(t, l, "k1"); got != (row{"right", original, 9}) {
		t.Fatalf("after a longer copy: %+v", got)
	}
}

// A fast-mode request costs its model's fast multiple; the rest do not.
func TestLedgerPricesFastMode(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.jsonl"), "k1 s 100 true\nk2 s 100 false\n")
	l := openTestLedger(t, root, rates{"m": {input: 0.000001, output: 0.00001, fast: 2}})
	h, err := l.History(t.Context(), 0, time.Hour.Milliseconds())
	if err != nil {
		t.Fatal(err)
	}
	// Each: 10 in at $1/M + 100 out at $10/M = 1010 micros; the fast one twice that.
	if len(h.Buckets) != 1 || h.Buckets[0].CostUSDMicros != 3*1010 {
		t.Fatalf("buckets: %+v", h.Buckets)
	}
	report, err := l.report(t.Context(), time.UnixMilli(1_000).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Buckets) != 1 || report.Buckets[0].CostUSDMicros != 3*1010 {
		t.Fatalf("report: %+v", report.Buckets)
	}
}

// Only litellm publishes fast tiers; its multiplier holds when models.dev
// prices the model, and a model without one prices fast at 1x.
func TestMergeRatesKeepsLitellmFastTier(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	lite := map[string]liteEntry{
		"claude-opus-x": {Input: f(0.000005), Output: f(0.000025), Specific: map[string]any{"us": 1.1, "fast": 2.0}},
		"claude-plain":  {Input: f(0.000001), Output: f(0.000002)},
	}
	dev := map[string]devProvider{"anthropic": {Models: map[string]struct {
		Cost *devCost `json:"cost"`
	}{"claude-opus-x": {Cost: &devCost{Input: f(4), Output: f(20)}}}}}
	got := mergeRates(lite, dev)
	if r := got["claude-opus-x"]; r.fast != 2 || r.input != 0.000004 {
		t.Fatalf("opus: %+v", r)
	}
	if r := got["claude-plain"]; r.fast != 1 {
		t.Fatalf("plain: %+v", r)
	}
}
