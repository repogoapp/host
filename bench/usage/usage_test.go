package usagebench

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/shipping"
)

const sessions = 1000
const requestsPerSession = 100

var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Only transcript roots are replaced; parsing and ledger queries use production code.
type fixtureProvider struct {
	shipping.Provider
	root string
}

func (p fixtureProvider) UsageRoots() []string { return []string{p.root} }

func fixture(b *testing.B) ([]shipping.Provider, string) {
	b.Helper()
	root := b.TempDir()
	providers := []shipping.Provider{
		fixtureProvider{Provider: &claude.Provider{}, root: filepath.Join(root, "claude")},
		fixtureProvider{Provider: &codex.Provider{}, root: filepath.Join(root, "codex")},
	}
	for _, p := range providers {
		if err := os.MkdirAll(p.UsageRoots()[0], 0700); err != nil {
			b.Fatal(err)
		}
	}
	for session := 0; session < sessions; session++ {
		p := providers[session%2]
		path := filepath.Join(p.UsageRoots()[0], fmt.Sprintf("session-%d.jsonl", session))
		f, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		w := bufio.NewWriter(f)
		if p.Kind() == agent.Kind("codex") {
			fmt.Fprintf(w, "{\"type\":\"session_meta\",\"payload\":{\"id\":\"session-%d\"}}\n", session)
			fmt.Fprintln(w, `{"type":"turn_context","payload":{"model":"bench-model"}}`)
		}
		for request := 0; request < requestsPerSession; request++ {
			at := start.AddDate(0, 0, (session*requestsPerSession+request)%365).Add(time.Duration(request) * time.Minute).Format(time.RFC3339)
			if p.Kind() == agent.Kind("claude") {
				fmt.Fprintf(w, "{\"type\":\"assistant\",\"timestamp\":%q,\"sessionId\":\"session-%d\",\"requestId\":\"request-%d-%d\",\"message\":{\"model\":\"bench-model\",\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":4000,\"cache_creation_input_tokens\":50,\"output_tokens\":500}}}\n", at, session, session, request)
			} else {
				fmt.Fprintf(w, "{\"type\":\"event_msg\",\"timestamp\":%q,\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":4150,\"cached_input_tokens\":4000,\"cache_write_input_tokens\":50,\"output_tokens\":500,\"reasoning_output_tokens\":100},\"total_token_usage\":{\"total_tokens\":%d}}}}\n", at, (request+1)*4650)
			}
		}
		if err := w.Flush(); err != nil {
			b.Fatal(err)
		}
		if err := f.Close(); err != nil {
			b.Fatal(err)
		}
	}
	rates := filepath.Join(root, "rates.json")
	body := fmt.Sprintf(`{"fetchedAtMs":%d,"rates":{"bench-model":{"inputCostPerToken":0.000003,"outputCostPerToken":0.000015,"cacheReadCostPerToken":0.0000003,"cacheCreationCostPerToken":0.00000375,"fastMultiplier":1}}}`, time.Now().UnixMilli())
	if err := os.WriteFile(rates, []byte(body), 0600); err != nil {
		b.Fatal(err)
	}
	return providers, rates
}

func open(b *testing.B, path, rates string, providers []shipping.Provider) *shipping.Ledger {
	b.Helper()
	ledger, err := shipping.Open(b.Context(), path, rates, providers...)
	if err != nil {
		b.Fatal(err)
	}
	return ledger
}

func checkYear(b *testing.B, h shipping.History) {
	b.Helper()
	var requests, input, output int64
	for _, bucket := range h.Buckets {
		requests += bucket.Requests
		input += bucket.InputTokens + bucket.CacheReadTokens + bucket.CacheWriteTokens
		output += bucket.OutputTokens
	}
	if !h.Complete || requests != sessions*requestsPerSession || input != requests*4150 || output != requests*500 || len(h.Pricing.UnpricedModels) != 0 {
		b.Fatalf("incorrect aggregate: complete=%v requests=%d input=%d output=%d", h.Complete, requests, input, output)
	}
}

func BenchmarkColdHistoryYear(b *testing.B) {
	providers, rates := fixture(b)
	dbRoot := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		ledger := open(b, filepath.Join(dbRoot, fmt.Sprintf("usage-%d.db", i)), rates, providers)
		b.StartTimer()
		h, err := ledger.History(b.Context(), start.UnixMilli(), start.AddDate(1, 0, 0).UnixMilli())
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
		checkYear(b, h)
		if err := ledger.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWarmHistory(b *testing.B) {
	providers, rates := fixture(b)
	ledger := open(b, filepath.Join(b.TempDir(), "usage.db"), rates, providers)
	b.Cleanup(func() { ledger.Close() })
	until := start.AddDate(1, 0, 0)
	h, err := ledger.History(b.Context(), start.UnixMilli(), until.UnixMilli())
	if err != nil {
		b.Fatal(err)
	}
	checkYear(b, h)
	for _, days := range []int{7, 30, 90, 365} {
		b.Run(fmt.Sprintf("%dd", days), func(b *testing.B) {
			since := until.AddDate(0, 0, -days)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := ledger.History(b.Context(), since.UnixMilli(), until.UnixMilli()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
