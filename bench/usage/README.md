# Usage ledger benchmark

Measures the production Go Claude/Codex usage parsers, SQLite ledger and
`shipping.Ledger.History` aggregation using generated transcripts. It uses
1,000 sessions, 100 requests each, split equally between Claude and Codex,
and spreads 100,000 requests across 365 days. Each request has 100 uncached
input, 4,000 cache-read, 50 cache-write and 500 output tokens. Codex output
includes 100 reasoning tokens. Aggregate checks verify that input/output
normalization preserves those totals and that the first index is complete.

All files and databases live in Go benchmark temporary directories. The
providers use their zero-value parsers with overridden transcript roots;
no agent login, personal transcripts or production database is read. A
fresh generated public-rate cache prevents network pricing requests.

From the host's root:

```sh
go test ./bench/usage -run '^$' -bench BenchmarkColdHistoryYear -benchtime=1x -count=3
go test ./bench/usage -run '^$' -bench BenchmarkWarmHistory -benchtime=1s -count=3
```

Cold timing includes discovering and parsing the fixtures, writing request
rows and querying a full year. Fixture generation and database creation are
outside the timer. Warm timing measures the public History call after the
index is populated, including grouping, pricing, session counts and sorting.

These are synthetic Go-path measurements, not a production-history replay.
They exclude native Rust parsing, network/phone latency, UI rendering,
large tool-result payloads, forks and duplicate transcript copies. Prompt-word
and task attribution are not implemented or measured by this benchmark.

## Results — 2026-09-30

Apple M3 Pro, Darwin arm64, Go benchmark suffix `-11`; three runs per case.
No production code optimization was made: this is a baseline, not a speedup.

| Case | Median | Range |
| --- | ---: | ---: |
| Cold index + year history, 100,000 requests | 1.024 s | 0.995–1.025 s |
| Warm 7-day history | 3.66 ms | 3.60–3.69 ms |
| Warm 30-day history | 15.06 ms | 14.68–15.15 ms |
| Warm 90-day history | 46.08 ms | 45.44–47.27 ms |
| Warm 365-day history | 205.77 ms | 205.01–207.59 ms |

Both benchmark commands passed their aggregate checks. Warm allocations were
approximately 20.8 KB / 516 allocations for a week, 88.2 KB / 1,856 for a month,
231 KB / 5,338 for 90 days and 921 KB / 21,298 for a year.

## Data available for prompt/output analysis

- `internal/shipping/usage.go`: request records include session identity,
  timestamp, model and input/cache/output/reasoning counts, but no user prompt
  identifier or word count.
- `internal/shipping/history.go`: the phone receives hourly agent/model sums
  and session counts, not individual prompts or task attribution.
- `internal/agents/claude/sessions.go` and `codex/sessions.go`: transcript parsing
  distinguishes user messages, assistant text and tool results, filters
  synthetic messages, and preserves turn IDs where supplied.

A next change could aggregate user-message counts and filtered prompt-word
counts on the host, then expose output tokens per user prompt and total usage
per user prompt. These should first be period-level averages: accurate
per-task attribution needs request-to-prompt links, treatment of steering,
subagents and missing historical turn IDs. Visible assistant words and billed
output tokens are different measures; billed output can include reasoning
and tool-call generation. Input tokens are the whole model context, not just
what the person typed. Transcript deletion limits historical word backfills,
because the persistent usage ledger retains counts but not message text.
