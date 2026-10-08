package shipping

import (
	"cmp"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/repogo/host/internal/errkind"
)

const (
	// historyWait bounds how long an answer waits on a sweep; past it the
	// phone gets what is indexed and hears the rest is still coming.
	historyWait = 15 * time.Second
	// maxRange caps one history request.
	maxRange = 400 * 24 * time.Hour
	hourMs   = int64(time.Hour / time.Millisecond)
)

// ErrIncomplete: a partial year would overwrite complete rows downstream, so
// none is served until the first index has finished.
var ErrIncomplete = errkind.New(errkind.Unavailable, "usage is still being indexed; retry")

// ErrRange is a history request whose range is empty or too long.
var ErrRange = errkind.New(errkind.Invalid, "since_ms must be before until_ms, at most 400 days apart")

// History is what this machine's agents spent in a range, per UTC hour, agent
// and model: small enough to send, fine enough for the phone to group into
// its own local days and to add to other environments' answers.
type History struct {
	HostID       string `json:"host_id"`
	CapturedAtMs int64  `json:"captured_at_ms"`
	// False while the first index is still reading history.
	Complete bool          `json:"complete"`
	Homes    []Home        `json:"homes" wire:"array"`
	Buckets  []HourBucket  `json:"buckets" wire:"array"`
	Sessions []AgentCount  `json:"sessions" wire:"array"`
	Pricing  PricingStatus `json:"pricing"`
}

// Home is a folder an agent's transcripts are read from; two environments
// naming the same machine and home read the same files.
type Home struct {
	Agent string `json:"agent"`
	Path  string `json:"path"`
}

// HourBucket is one agent's use of one model in one UTC hour.
type HourBucket struct {
	Hour  int64  `json:"hour_ms"`
	Agent string `json:"agent"`
	Model string `json:"model"`
	// What a phone shows for Model; stamped by the usage.history method.
	ModelLabel string `json:"model_label"`
	// Uncached input only.
	InputTokens      int64 `json:"input_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	// Includes ReasoningTokens.
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	Requests        int64 `json:"requests"`
	// At public API rates; zero with Priced false when the model has none.
	CostUSDMicros         int64 `json:"cost_usd_micros"`
	CacheSavingsUSDMicros int64 `json:"cache_savings_usd_micros"`
	Priced                bool  `json:"priced"`
}

// AgentCount is how many chats an agent billed in the range.
type AgentCount struct {
	Agent string `json:"agent"`
	Count int64  `json:"count"`
}

// PricingStatus says how current the rates are and which models had none.
type PricingStatus struct {
	FetchedAtMs    int64    `json:"fetched_at_ms"`
	UnpricedModels []string `json:"unpriced_models" wire:"array"`
}

// History answers a range, sweeping first when the ledger is over a minute old.
func (l *Ledger) History(ctx context.Context, sinceMs, untilMs int64) (History, error) {
	if sinceMs >= untilMs || untilMs-sinceMs > maxRange.Milliseconds() {
		return History{}, ErrRange
	}
	wait, cancel := context.WithTimeout(ctx, historyWait)
	l.refresh(wait, freshFor)
	cancel()
	now := time.Now()
	rates := l.pricing.table(ctx, now)

	rows, err := l.db.QueryContext(ctx, `SELECT at / ? * ? AS hour, agent, model, reported_micros IS NOT NULL, fast,
		  SUM(input), SUM(cache_read), SUM(cache_write_5m), SUM(cache_write_1h), SUM(output), SUM(reasoning),
		  COUNT(*), SUM(COALESCE(reported_micros, 0))
		FROM requests WHERE at >= ? AND at < ?
		GROUP BY hour, agent, model, reported_micros IS NOT NULL, fast`, hourMs, hourMs, sinceMs, untilMs)
	if err != nil {
		return History{}, err
	}
	type key struct {
		hour         int64
		agent, model string
	}
	byKey := map[key]*HourBucket{}
	unpriced := map[string]bool{}
	for rows.Next() {
		var k key
		var reported, fast bool
		var t Tokens
		var requests, reportedMicros int64
		if err := rows.Scan(&k.hour, &k.agent, &k.model, &reported, &fast,
			&t.Uncached, &t.Cached, &t.Creation, &t.Creation1h, &t.Output, &t.Reasoning,
			&requests, &reportedMicros); err != nil {
			rows.Close()
			return History{}, err
		}
		b := byKey[k]
		if b == nil {
			b = &HourBucket{Hour: k.hour, Agent: k.agent, Model: k.model, Priced: true}
			byKey[k] = b
		}
		b.InputTokens += t.Uncached
		b.CacheReadTokens += t.Cached
		b.CacheWriteTokens += t.Creation + t.Creation1h
		b.OutputTokens += t.Output
		b.ReasoningTokens += t.Reasoning
		b.Requests += requests
		rate, ok := rates.rate(k.model)
		if ok {
			b.CacheSavingsUSDMicros += micros(rate.savings(t))
		}
		switch {
		case reported:
			b.CostUSDMicros += reportedMicros
		case ok:
			b.CostUSDMicros += micros(rate.costOf(t, fast))
		default:
			b.Priced = false
			unpriced[cmp.Or(k.model, "unknown")] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return History{}, err
	}

	sessions, err := l.sessions(ctx, sinceMs, untilMs)
	if err != nil {
		return History{}, err
	}
	out := History{
		HostID:       hostID(),
		CapturedAtMs: now.UnixMilli(),
		Complete:     l.indexed(),
		Homes:        l.homes(),
		Buckets:      make([]HourBucket, 0, len(byKey)),
		Sessions:     sessions,
		Pricing:      PricingStatus{FetchedAtMs: l.pricing.fetchedAtMs(), UnpricedModels: []string{}},
	}
	for _, b := range byKey {
		out.Buckets = append(out.Buckets, *b)
	}
	slices.SortFunc(out.Buckets, func(x, y HourBucket) int {
		return cmp.Or(cmp.Compare(x.Hour, y.Hour), strings.Compare(x.Agent, y.Agent), strings.Compare(x.Model, y.Model))
	})
	for model := range unpriced {
		out.Pricing.UnpricedModels = append(out.Pricing.UnpricedModels, model)
	}
	slices.Sort(out.Pricing.UnpricedModels)
	return out, nil
}

func (l *Ledger) sessions(ctx context.Context, sinceMs, untilMs int64) ([]AgentCount, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT agent, COUNT(DISTINCT session) FROM requests
		WHERE at >= ? AND at < ? AND session != '' GROUP BY agent ORDER BY agent`, sinceMs, untilMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentCount{}
	for rows.Next() {
		var c AgentCount
		if err := rows.Scan(&c.Agent, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// homes is each agent's transcript folder, once per agent.
func (l *Ledger) homes() []Home {
	out := []Home{}
	seen := map[Home]bool{}
	for _, p := range l.providers {
		for _, root := range p.UsageRoots() {
			h := Home{Agent: string(p.Kind()), Path: filepath.Dir(root)}
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// Report is the leaderboard's calendar year so far in the host's zone.
type Report struct {
	HostID       string   `json:"host_id"`
	CapturedAtMs int64    `json:"captured_at_ms"`
	TimeZone     string   `json:"time_zone"`
	Buckets      []Bucket `json:"buckets" wire:"array"`
}

// Bucket is one provider's local day, summed across models.
type Bucket struct {
	Day      string `json:"day"`
	Provider string `json:"provider"`
	// Uncached, cache-read and cache-creation input together.
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// API-equivalent cost at public rates; zero for a model with no known rate.
	CostUSDMicros int64 `json:"cost_usd_micros"`
}

// Report restates the whole calendar year each time: the yearly board is
// complete only if every report does.
func (l *Ledger) Report(ctx context.Context) (Report, error) {
	wait, cancel := context.WithTimeout(ctx, historyWait)
	l.refresh(wait, freshFor)
	cancel()
	return l.report(ctx, time.Now())
}

// Scan sweeps, however fresh the ledger, and reports now's year: what a
// caller that needs the files as they are this moment uses.
func (l *Ledger) Scan(ctx context.Context, now time.Time) (Report, error) {
	l.refresh(ctx, 0)
	return l.report(ctx, now)
}

// report buckets now's calendar year, Jan 1 through today, in now's zone,
// from what the ledger holds; ErrIncomplete until the first index finished.
func (l *Ledger) report(ctx context.Context, now time.Time) (Report, error) {
	if !l.indexed() {
		return Report{}, ErrIncomplete
	}
	rates := l.pricing.table(ctx, now)
	loc := now.Location()
	yearStart := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, loc)
	dayEnd := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, loc)

	rows, err := l.db.QueryContext(ctx, `SELECT at, agent, model, input, cache_read, cache_write_5m, cache_write_1h, output, reasoning, fast, reported_micros
		FROM requests WHERE at >= ? AND at < ?`, yearStart.UnixMilli(), dayEnd.UnixMilli())
	if err != nil {
		return Report{}, err
	}
	type key struct{ provider, model, day string }
	type sum struct {
		tokens Tokens
		usd    float64
	}
	byModel := map[key]*sum{}
	for rows.Next() {
		var at int64
		var provider, model string
		var t Tokens
		var fast bool
		var reported *int64
		if err := rows.Scan(&at, &provider, &model, &t.Uncached, &t.Cached, &t.Creation, &t.Creation1h, &t.Output, &t.Reasoning, &fast, &reported); err != nil {
			rows.Close()
			return Report{}, err
		}
		k := key{provider, model, time.UnixMilli(at).In(loc).Format("2006-01-02")}
		s := byModel[k]
		if s == nil {
			s = &sum{}
			byModel[k] = s
		}
		s.tokens.add(t)
		if reported != nil {
			s.usd += float64(*reported) / 1_000_000
		} else if rate, ok := rates.rate(model); ok {
			s.usd += rate.costOf(t, fast)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Report{}, err
	}

	// Rounded per model before summing, as v1's buckets were, so a day keys the same total.
	type dayKey struct{ provider, day string }
	byDay := map[dayKey]*Bucket{}
	for k, s := range byModel {
		b := byDay[dayKey{k.provider, k.day}]
		if b == nil {
			b = &Bucket{Day: k.day, Provider: k.provider}
			byDay[dayKey{k.provider, k.day}] = b
		}
		b.InputTokens += s.tokens.Uncached + s.tokens.Cached + s.tokens.Creation + s.tokens.Creation1h
		b.OutputTokens += s.tokens.Output
		b.CostUSDMicros += micros(s.usd)
	}
	buckets := make([]Bucket, 0, len(byDay))
	for _, b := range byDay {
		buckets = append(buckets, *b)
	}
	slices.SortFunc(buckets, func(x, y Bucket) int {
		return cmp.Or(strings.Compare(x.Day, y.Day), strings.Compare(x.Provider, y.Provider))
	})
	return Report{
		HostID:       hostID(),
		CapturedAtMs: now.UnixMilli(),
		TimeZone:     zoneName(loc),
		Buckets:      buckets,
	}, nil
}

func (t *Tokens) add(o Tokens) {
	t.Uncached += o.Uncached
	t.Cached += o.Cached
	t.Creation += o.Creation
	t.Creation1h += o.Creation1h
	t.Output += o.Output
	t.Reasoning += o.Reasoning
}
