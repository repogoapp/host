package agents_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	claudeprovider "github.com/repogo/host/internal/agents/claude"
	codexprovider "github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/shipping"
)

func TestUsageSumsEachProviderDayAndPricesIt(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	base := t.TempDir()
	claude, codex, cache := filepath.Join(base, "claude"), filepath.Join(base, "codex"), filepath.Join(t.TempDir(), "rates.json")

	writeLines(t, filepath.Join(claude, "projects", "p", "s.jsonl"),
		`{"type":"assistant","timestamp":"2026-03-10T12:00:00Z","requestId":"r1","message":{"id":"m1","model":"claude-test","content":"not counted","usage":{"input_tokens":100,"cache_read_input_tokens":50,"cache_creation_input_tokens":25,"output_tokens":10}}}`,
		// The same response restated: counted once.
		`{"type":"assistant","timestamp":"2026-03-10T12:00:01Z","requestId":"r1","message":{"id":"m1","model":"claude-test","usage":{"input_tokens":100,"cache_read_input_tokens":50,"cache_creation_input_tokens":25,"output_tokens":10}}}`,
		// A provider-reported cost wins over the rate table.
		`{"type":"assistant","timestamp":"2026-03-10T13:00:00Z","requestId":"r2","costUSD":0.5,"message":{"id":"m2","model":"claude-test","usage":{"input_tokens":10,"output_tokens":1}}}`,
		// Last year and the future are outside the board's year.
		`{"type":"assistant","timestamp":"2025-12-31T12:00:00Z","requestId":"r3","message":{"id":"m3","model":"claude-test","usage":{"input_tokens":7,"output_tokens":7}}}`,
		`{"type":"assistant","timestamp":"2026-07-01T12:00:00Z","requestId":"r4","message":{"id":"m4","model":"claude-test","usage":{"input_tokens":7,"output_tokens":7}}}`,
		`{"type":"user","message":{"content":"the word \"usage\" in a prompt"}}`,
	)
	writeLines(t, filepath.Join(codex, "sessions", "2026", "03", "10", "rollout.jsonl"),
		`{"type":"session_meta","payload":{"id":"s1"}}`,
		`{"type":"turn_context","payload":{"model":"gpt-test"}}`,
		`{"type":"event_msg","timestamp":"2026-03-10T15:00:00Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":400,"output_tokens":100,"reasoning_output_tokens":20}}}}`,
		// Codex restamps the same last usage: counted once.
		`{"type":"event_msg","timestamp":"2026-03-10T15:00:05Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":400,"output_tokens":100,"reasoning_output_tokens":20}}}}`,
		`{"type":"event_msg","timestamp":"2026-03-11T01:00:00Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":200,"output_tokens":50}}}}`,
	)
	writeLines(t, cache, `{"version":2,"fetchedAtMs":`+jsonInt(now.UnixMilli())+`,"rates":{`+
		`"claude-test":{"inputCostPerToken":0.000001,"outputCostPerToken":0.000002,"cacheReadCostPerToken":0.0000001,"cacheCreationCostPerToken":0.00000125},`+
		`"gpt-test":{"inputCostPerToken":0.000002,"outputCostPerToken":0.000008,"cacheReadCostPerToken":0.0000005,"cacheCreationCostPerToken":0.000002}}}`)

	report, err := openLedger(t, cache, claudeprovider.New(agent.Dependencies{Root: base}), codexprovider.New(agent.Dependencies{Root: base})).Scan(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []shipping.Bucket{
		// 100+50+25 + 10 in; 156.25 micros at rates plus the reported $0.50.
		{Day: "2026-03-10", Provider: "claude", InputTokens: 185, OutputTokens: 11, CostUSDMicros: 500_156},
		// 600 uncached + 400 cached in: 1200 + 200 + 800 micros.
		{Day: "2026-03-10", Provider: "codex", InputTokens: 1000, OutputTokens: 100, CostUSDMicros: 2_200},
		{Day: "2026-03-11", Provider: "codex", InputTokens: 200, OutputTokens: 50, CostUSDMicros: 800},
	}
	if !reflect.DeepEqual(report.Buckets, want) {
		t.Fatalf("buckets:\n got %+v\nwant %+v", report.Buckets, want)
	}
	if len(report.MachineID) != 16 || report.TimeZone != "UTC" || report.CapturedAtMs != now.UnixMilli() {
		t.Fatalf("envelope: %+v", report)
	}

	b, _ := json.Marshal(report)
	for _, key := range []string{`"machine_id"`, `"captured_at_ms"`, `"time_zone"`, `"buckets"`, `"day"`, `"provider"`, `"input_tokens"`, `"output_tokens"`, `"cost_usd_micros"`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("missing %s in %s", key, b)
		}
	}
	if strings.Contains(string(b), "not counted") || strings.Contains(string(b), "claude-test") {
		t.Fatalf("content or model leaked: %s", b)
	}
}

func TestUsageDaysAreLocal(t *testing.T) {
	zone := time.FixedZone("UTC-5", -5*60*60)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, zone)
	base := t.TempDir()
	claude := filepath.Join(base, "claude")
	// 03:00 UTC on Mar 11 is still Mar 10 five hours west.
	writeLines(t, filepath.Join(claude, "projects", "p", "s.jsonl"),
		`{"type":"assistant","timestamp":"2026-03-11T03:00:00Z","requestId":"r1","message":{"id":"m1","model":"unpriced","usage":{"input_tokens":1,"output_tokens":1}}}`,
		// Jan 1 01:00 UTC is Dec 31 locally: last year.
		`{"type":"assistant","timestamp":"2026-01-01T01:00:00Z","requestId":"r2","message":{"id":"m2","model":"unpriced","usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
	cache := filepath.Join(t.TempDir(), "rates.json")
	writeLines(t, cache, `{"version":2,"fetchedAtMs":`+jsonInt(now.UnixMilli())+`,"rates":{}}`)
	report, err := openLedger(t, cache, claudeprovider.New(agent.Dependencies{Root: base})).Scan(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []shipping.Bucket{{Day: "2026-03-10", Provider: "claude", InputTokens: 1, OutputTokens: 1}}
	if !reflect.DeepEqual(report.Buckets, want) {
		t.Fatalf("buckets: %+v", report.Buckets)
	}
}

func openLedger(t *testing.T, rates string, providers ...shipping.Provider) *shipping.Ledger {
	t.Helper()
	l, err := shipping.Open(t.Context(), filepath.Join(t.TempDir(), "usage.db"), rates, providers...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// A resumed Claude chat and a forked Codex rollout copy their parent's
// requests; each request counts once, under the model that served it, and a
// subagent's transcript bills too.
func TestUsageHistoryCountsCopiesOnceByModel(t *testing.T) {
	base := t.TempDir()
	claude, codex := filepath.Join(base, "claude"), filepath.Join(base, "codex")
	cache := filepath.Join(t.TempDir(), "rates.json")
	writeLines(t, cache, `{"version":2,"fetchedAtMs":`+jsonInt(time.Now().UnixMilli())+`,"rates":{`+
		`"claude-opus-x":{"inputCostPerToken":0.000001,"outputCostPerToken":0.000002,"cacheReadCostPerToken":0.0000001,"cacheCreationCostPerToken":0.00000125}}}`)

	usageFixtures(t, base)

	l := openLedger(t, cache, claudeprovider.New(agent.Dependencies{Root: base}), codexprovider.New(agent.Dependencies{Root: base}))
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	h, err := l.History(t.Context(), since.UnixMilli(), since.Add(24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		agent, model                   string
		requests, input, cache, output int64
	}
	var got []row
	for _, b := range h.Buckets {
		got = append(got, row{b.Agent, b.Model, b.Requests, b.InputTokens, b.CacheReadTokens + b.CacheWriteTokens, b.OutputTokens})
	}
	want := []row{
		// m1, m3 and the subagent's m4; m1 and m2 once though two files hold them.
		{"claude", "claude-opus-x", 3, 13, 100, 8},
		{"claude", "claude-sonnet-x", 1, 7, 0, 20},
		// The parent's two requests; the fork's copies and repeat are not counted again.
		{"codex", "gpt-a", 1, 50, 30, 20},
		// The parent's second request, and the user fork's own.
		{"codex", "gpt-b", 2, 170, 0, 40},
		{"codex", "gpt-c", 1, 100, 0, 50},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buckets:\n got %+v\nwant %+v", got, want)
	}
	for _, b := range h.Buckets {
		switch b.Model {
		case "claude-opus-x":
			// m1: 10 in + 100 hour-long cache writes at twice input + 5 out; m3 and m4: 3 in + 3 out.
			if !b.Priced || b.CostUSDMicros != 10+200+10+3+6 {
				t.Fatalf("opus cost: %+v", b)
			}
		default:
			if b.Priced || b.CostUSDMicros != 0 {
				t.Fatalf("%s should be unpriced: %+v", b.Model, b)
			}
		}
		if b.Hour%time.Hour.Milliseconds() != 0 {
			t.Fatalf("hour not on the hour: %+v", b)
		}
	}
	// The subagent's requests count under its parent; the user fork is its own chat.
	if want := []shipping.AgentCount{{Agent: "claude", Count: 2}, {Agent: "codex", Count: 2}}; !reflect.DeepEqual(h.Sessions, want) {
		t.Fatalf("sessions: %+v", h.Sessions)
	}
	if !h.Complete || len(h.MachineID) != 16 || len(h.Homes) != 2 || h.Homes[0].Path != claude || h.Homes[1].Path != codex {
		t.Fatalf("envelope: %+v", h)
	}
	if want := []string{"claude-sonnet-x", "gpt-a", "gpt-b", "gpt-c"}; !reflect.DeepEqual(h.Pricing.UnpricedModels, want) {
		t.Fatalf("unpriced: %v", h.Pricing.UnpricedModels)
	}
}

// Agents delete old transcripts; the ledger keeps what they billed, and an
// appended file is read again.
func TestUsageHistoryOutlivesTranscripts(t *testing.T) {
	base := t.TempDir()
	claude := filepath.Join(base, "claude")
	cache := filepath.Join(t.TempDir(), "rates.json")
	writeLines(t, cache, `{"version":2,"fetchedAtMs":`+jsonInt(time.Now().UnixMilli())+`,"rates":{}}`)
	line := func(id string) string {
		return `{"type":"assistant","timestamp":"2026-09-01T10:00:00Z","sessionId":"s","requestId":"` + id + `","message":{"id":"` + id + `","model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`
	}
	gone := filepath.Join(claude, "projects", "p", "gone.jsonl")
	kept := filepath.Join(claude, "projects", "p", "kept.jsonl")
	writeLines(t, gone, line("a"))
	writeLines(t, kept, line("b"))

	l := openLedger(t, cache, claudeprovider.New(agent.Dependencies{Root: base}))
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	if _, err := l.Scan(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	writeLines(t, kept, line("b"), line("c"))
	report, err := l.Scan(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []shipping.Bucket{{Day: "2026-09-01", Provider: "claude", InputTokens: 3, OutputTokens: 3}}
	if !reflect.DeepEqual(report.Buckets, want) {
		t.Fatalf("buckets: %+v", report.Buckets)
	}
}

// usageFixtures writes transcripts that copy requests the ways the agents do:
// a resumed Claude chat, a streamed reply, a model switched mid-turn, a
// subagent, and a Codex subagent rollout opening with its parent's history.
func usageFixtures(t *testing.T, base string) {
	t.Helper()
	claude, codex := filepath.Join(base, "claude"), filepath.Join(base, "codex")
	m1 := `{"type":"assistant","timestamp":"2026-09-01T10:00:00Z","sessionId":"sA","requestId":"r1","message":{"id":"m1","model":"claude-opus-x","usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":100},"output_tokens":5}}}`
	// A streamed reply's early line, then its last; the model switched mid-turn.
	m2early := `{"type":"assistant","timestamp":"2026-09-01T10:01:00Z","sessionId":"sA","requestId":"r2","message":{"id":"m2","model":"claude-sonnet-x","usage":{"input_tokens":7,"output_tokens":1}}}`
	m2 := `{"type":"assistant","timestamp":"2026-09-01T10:01:00Z","sessionId":"sA","requestId":"r2","message":{"id":"m2","model":"claude-sonnet-x","usage":{"input_tokens":7,"output_tokens":20}}}`
	writeLines(t, filepath.Join(claude, "projects", "p", "a.jsonl"), m1, m2early, m2,
		`{"type":"assistant","timestamp":"2026-09-01T10:02:00Z","sessionId":"sA","message":{"id":"s1","model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0}}}`)
	// The resume copies m1 and m2 under its own session, then asks once more.
	writeLines(t, filepath.Join(claude, "projects", "p", "b.jsonl"),
		strings.ReplaceAll(m1, `"sA"`, `"sB"`), strings.ReplaceAll(m2, `"sA"`, `"sB"`),
		`{"type":"assistant","timestamp":"2026-09-01T10:03:00Z","sessionId":"sB","requestId":"r3","message":{"id":"m3","model":"claude-opus-x","usage":{"input_tokens":1,"output_tokens":1}}}`)
	writeLines(t, filepath.Join(claude, "projects", "p", "sA", "subagents", "agent-x.jsonl"),
		`{"type":"assistant","isSidechain":true,"timestamp":"2026-09-01T10:04:00Z","sessionId":"sA","requestId":"r4","message":{"id":"m4","model":"claude-opus-x","usage":{"input_tokens":2,"output_tokens":2}}}`)

	count := func(ts string, total, input, cached, output int) string {
		return `{"type":"event_msg","timestamp":"` + ts + `","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":` + jsonInt(int64(total)) + `},"last_token_usage":{"input_tokens":` + jsonInt(int64(input)) + `,"cached_input_tokens":` + jsonInt(int64(cached)) + `,"output_tokens":` + jsonInt(int64(output)) + `}}}}`
	}
	turn := func(ts, model string) string {
		return `{"type":"turn_context","timestamp":"` + ts + `","payload":{"model":"` + model + `"}}`
	}
	writeLines(t, filepath.Join(codex, "sessions", "2026", "09", "01", "rollout-p.jsonl"),
		`{"type":"session_meta","timestamp":"2026-09-01T11:00:00Z","payload":{"id":"p"}}`,
		turn("2026-09-01T11:00:00Z", "gpt-a"),
		count("2026-09-01T11:00:10Z", 100, 80, 30, 20),
		// Restated on a stream boundary: the same total.
		count("2026-09-01T11:00:11Z", 100, 80, 30, 20),
		turn("2026-09-01T11:01:00Z", "gpt-b"),
		count("2026-09-01T11:01:10Z", 250, 120, 0, 30),
	)
	// A subagent's rollout opens with the parent's history re-stamped to one
	// instant, repeats the last count a moment later, then does its own work.
	writeLines(t, filepath.Join(codex, "sessions", "2026", "09", "01", "rollout-f.jsonl"),
		`{"type":"session_meta","timestamp":"2026-09-01T11:05:00.000Z","payload":{"id":"f","forked_from_id":"p","source":{"subagent":{"thread_spawn":{"parent_thread_id":"p"}}}}}`,
		`{"type":"session_meta","timestamp":"2026-09-01T11:05:00.000Z","payload":{"id":"p"}}`,
		turn("2026-09-01T11:05:00.000Z", "gpt-a"),
		count("2026-09-01T11:05:00.000Z", 100, 80, 30, 20),
		turn("2026-09-01T11:05:00.002Z", "gpt-b"),
		count("2026-09-01T11:05:00.004Z", 250, 120, 0, 30),
		count("2026-09-01T11:05:02.000Z", 250, 120, 0, 30),
		turn("2026-09-01T11:05:04Z", "gpt-c"),
		count("2026-09-01T11:05:09Z", 400, 100, 0, 50),
	)
	// A fork the user made is a chat of its own; it copies the parent's first request.
	writeLines(t, filepath.Join(codex, "sessions", "2026", "09", "01", "rollout-u.jsonl"),
		`{"type":"session_meta","timestamp":"2026-09-01T11:10:00.000Z","payload":{"id":"u","forked_from_id":"p"}}`,
		turn("2026-09-01T11:10:00.000Z", "gpt-a"),
		count("2026-09-01T11:10:00.001Z", 100, 80, 30, 20),
		turn("2026-09-01T11:10:03Z", "gpt-b"),
		count("2026-09-01T11:10:06Z", 300, 50, 0, 10),
	)

}
