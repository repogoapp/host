package shipping

import (
	"cmp"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/errkind"
)

const (
	// historyWait bounds how long an answer waits on a sweep; past it the
	// phone gets what is indexed and hears the rest is still coming.
	historyWait = 15 * time.Second
	// maxRange caps one history request.
	maxRange = 400 * 24 * time.Hour
	hourMs   = int64(time.Hour / time.Millisecond)
)

// ErrRange is a history request whose range is empty or too long.
var ErrRange = errkind.New(errkind.Invalid, "since_ms must be before until_ms, at most 400 days apart")

// History is what this machine's agents spent in a range, per UTC hour, agent
// and model: small enough to send, fine enough for the phone to group into
// its own local days and to add to other environments' answers.
type History struct {
	// Two environments on one machine share it, so the phone counts a home
	// both read once.
	MachineID string `json:"machine_id"`
	// False while the first index is still reading history.
	Complete bool         `json:"complete"`
	Homes    []Home       `json:"homes" wire:"array"`
	Buckets  []HourBucket `json:"buckets" wire:"array"`
	Sessions []AgentCount `json:"sessions" wire:"array"`
	// False until public rates were fetched once; every bucket is unpriced till then.
	PricingFetched bool `json:"pricing_fetched"`
}

// Home is a folder an agent's transcripts are read from; two environments
// naming the same machine and home read the same files.
type Home struct {
	Agent string `json:"agent"`
	Path  string `json:"path"`
}

// HourBucket is one agent's use of one model in one UTC hour.
type HourBucket struct {
	HourMs int64  `json:"hour_ms"`
	Agent  string `json:"agent"`
	Model  string `json:"model"`
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

// Scope is which requests History counts. The zero Scope is all of them.
type Scope struct {
	folders []string
	// narrowed tells "these folders" from "everything", so a project with no
	// checkout here counts nothing rather than the whole machine.
	narrowed bool
}

// Folders narrows History to requests run in these folders or in one under
// them: a project's checkouts. No folders is no requests.
func Folders(paths ...string) Scope { return Scope{folders: paths, narrowed: true} }

// History answers a range, sweeping first when the ledger is over a minute old.
func (l *Ledger) History(ctx context.Context, sinceMs, untilMs int64, scope Scope) (History, error) {
	if sinceMs >= untilMs || untilMs-sinceMs > maxRange.Milliseconds() {
		return History{}, ErrRange
	}
	wait, cancel := context.WithTimeout(ctx, historyWait)
	l.refresh(wait, freshFor)
	cancel()
	rates := l.pricing.table(ctx, time.Now())

	where, args := inRange(sinceMs, untilMs, scope)
	rows, err := l.db.QueryContext(ctx, `SELECT at / ? * ? AS hour, agent, model, reported_micros IS NOT NULL, fast,
		  SUM(input), SUM(cache_read), SUM(cache_write_5m), SUM(cache_write_1h), SUM(output), SUM(reasoning),
		  COUNT(*), SUM(COALESCE(reported_micros, 0))
		FROM requests WHERE `+where+`
		GROUP BY hour, agent, model, reported_micros IS NOT NULL, fast`, append([]any{hourMs, hourMs}, args...)...)
	if err != nil {
		return History{}, err
	}
	type key struct {
		hour         int64
		agent, model string
	}
	byKey := map[key]*HourBucket{}
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
			b = &HourBucket{HourMs: k.hour, Agent: k.agent, Model: k.model, Priced: true}
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
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return History{}, err
	}

	sessions, err := l.sessions(ctx, where, args)
	if err != nil {
		return History{}, err
	}
	out := History{
		MachineID:      machineID(),
		Complete:       l.indexed(),
		Homes:          l.homes(),
		Buckets:        make([]HourBucket, 0, len(byKey)),
		Sessions:       sessions,
		PricingFetched: l.pricing.fetchedAtMs() > 0,
	}
	for _, b := range byKey {
		out.Buckets = append(out.Buckets, *b)
	}
	slices.SortFunc(out.Buckets, func(x, y HourBucket) int {
		return cmp.Or(cmp.Compare(x.HourMs, y.HourMs), strings.Compare(x.Agent, y.Agent), strings.Compare(x.Model, y.Model))
	})
	return out, nil
}

// inRange is the WHERE clause for a range within a scope. A folder and those
// under it match as an equality plus the range "/a/" to "/a0" ('0' follows
// '/'), which the (cwd, at) index serves without LIKE's escaping.
func inRange(sinceMs, untilMs int64, scope Scope) (string, []any) {
	if !scope.narrowed {
		return `at >= ? AND at < ?`, []any{sinceMs, untilMs}
	}
	var folders []string
	var args []any
	for _, path := range scope.folders {
		path = strings.TrimRight(path, "/")
		if path == "" {
			continue
		}
		folders = append(folders, `cwd = ? OR (cwd >= ? AND cwd < ?)`)
		args = append(args, path, path+"/", path+"0")
	}
	if len(folders) == 0 {
		return `0`, nil
	}
	return `(` + strings.Join(folders, ` OR `) + `) AND at >= ? AND at < ?`, append(args, sinceMs, untilMs)
}

func (l *Ledger) sessions(ctx context.Context, where string, args []any) ([]AgentCount, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT agent, COUNT(DISTINCT session) FROM requests
		WHERE `+where+` AND session != '' GROUP BY agent ORDER BY agent`, args...)
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
