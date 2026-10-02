package shipping

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/apphome"
)

// Public rates change rarely; a day keeps the fetch off every report.
const ratesTTL = 24 * time.Hour

type rate struct {
	input, output, cacheRead, cacheCreation float64
	// fast multiplies the whole cost of a fast-mode request; 1 for a model
	// with no fast tier.
	fast float64
}

// cost is what t costs at r. An hour-long cache write is twice the input
// rate, the way Anthropic prices it; the table quotes only the 5-minute kind.
func (r rate) cost(t Tokens) float64 {
	return float64(t.Uncached)*r.input + float64(t.Cached)*r.cacheRead +
		float64(t.Creation)*r.cacheCreation + float64(t.Creation1h)*r.input*2 + float64(t.Output)*r.output
}

// costOf is a request's cost at r, fast mode's multiplier included.
func (r rate) costOf(t Tokens, fast bool) float64 {
	if fast {
		return r.cost(t) * r.fast
	}
	return r.cost(t)
}

// savings is what reading t's cache saved over sending it as fresh input.
func (r rate) savings(t Tokens) float64 {
	return float64(t.Cached) * max(0, r.input-r.cacheRead)
}

type rates map[string]rate

func (r rates) rate(model string) (rate, bool) {
	found, ok := r[normalizeModel(model)]
	return found, ok
}

func normalizeModel(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(normalized, "/"); i >= 0 {
		normalized = normalized[i+1:]
	}
	return normalized
}

// pricing holds public per-token rates, cached on disk at path, behind its own
// lock so a download never holds up a scan's cache.
type pricing struct {
	path  string
	fetch func(context.Context) rates

	mu        sync.Mutex
	rates     rates
	fetchedAt time.Time
}

// table serves fresh rates, else refetches, else whatever it last had; an
// empty table leaves every model unpriced rather than failing the report.
func (p *pricing) table(ctx context.Context, now time.Time) rates {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fetchedAt.IsZero() {
		p.load()
	}
	// A stored table is valid by when it was fetched, even an empty one.
	if !p.fetchedAt.IsZero() && now.Sub(p.fetchedAt) < ratesTTL {
		return p.rates
	}
	if fresh := p.fetch(ctx); len(fresh) > 0 {
		p.rates, p.fetchedAt = fresh, now
		p.save()
	}
	return p.rates
}

// fetchedAtMs is when the rates in use were fetched, zero for none yet.
func (p *pricing) fetchedAtMs() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fetchedAt.IsZero() {
		return 0
	}
	return p.fetchedAt.UnixMilli()
}

// storedRates is the rates file, keyed by normalized model id.
type storedRates struct {
	FetchedAtMs int64                 `json:"fetchedAtMs"`
	Rates       map[string]storedRate `json:"rates"`
}

type storedRate struct {
	Input         float64 `json:"inputCostPerToken"`
	Output        float64 `json:"outputCostPerToken"`
	CacheRead     float64 `json:"cacheReadCostPerToken"`
	CacheCreation float64 `json:"cacheCreationCostPerToken"`
	Fast          float64 `json:"fastMultiplier"`
}

func (p *pricing) load() {
	var stored storedRates
	if found, err := apphome.ReadJSON(p.path, &stored); !found || err != nil || stored.FetchedAtMs <= 0 {
		return
	}
	p.rates = make(rates, len(stored.Rates))
	for model, r := range stored.Rates {
		p.rates[model] = rate{input: r.Input, output: r.Output, cacheRead: r.CacheRead, cacheCreation: r.CacheCreation, fast: max(1, r.Fast)}
	}
	p.fetchedAt = time.UnixMilli(stored.FetchedAtMs)
}

func (p *pricing) save() {
	stored := storedRates{FetchedAtMs: p.fetchedAt.UnixMilli(), Rates: make(map[string]storedRate, len(p.rates))}
	for model, r := range p.rates {
		stored.Rates[model] = storedRate{Input: r.input, Output: r.output, CacheRead: r.cacheRead, CacheCreation: r.cacheCreation, Fast: r.fast}
	}
	_ = apphome.WriteJSON(p.path, stored, 0o600)
}

// models.dev resells one id under several providers at different rates; the
// first party wins.
var preferredProviders = []string{"anthropic", "openai", "google"}

// liteEntry is one model in LiteLLM's price file, prices per token.
type liteEntry struct {
	Input         *float64 `json:"input_cost_per_token"`
	Output        *float64 `json:"output_cost_per_token"`
	CacheRead     *float64 `json:"cache_read_input_token_cost"`
	CacheCreation *float64 `json:"cache_creation_input_token_cost"`
	// Multipliers on the whole price, such as {"fast": 2} for a fast tier.
	Specific map[string]any `json:"provider_specific_entry"`
}

// devCost is one model's prices on models.dev, dollars per million tokens.
type devCost struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

type devProvider struct {
	Models map[string]struct {
		Cost *devCost `json:"cost"`
	} `json:"models"`
}

// download fetches both price sources, the same ones v1 priced from.
func download(ctx context.Context) rates {
	var lite map[string]liteEntry
	var modelsDev map[string]devProvider
	fetchJSON(ctx, "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json", &lite)
	fetchJSON(ctx, "https://models.dev/api.json", &modelsDev)
	return mergeRates(lite, modelsDev)
}

// mergeRates lays litellm (dated snapshot ids) under models.dev (current ids
// and aliases), models.dev winning. Only litellm publishes fast tiers, so its
// multipliers apply whichever source priced the model.
func mergeRates(lite map[string]liteEntry, modelsDev map[string]devProvider) rates {
	out := rates{}
	fast := map[string]float64{}
	for name, e := range lite {
		if e.Input == nil || e.Output == nil {
			continue
		}
		out[normalizeModel(name)] = withCache(*e.Input, *e.Output, e.CacheRead, e.CacheCreation, 1)
		if m, ok := e.Specific["fast"].(float64); ok && m > 0 {
			fast[normalizeModel(name)] = m
		}
	}

	var providers, rest []string
	for _, name := range preferredProviders {
		if _, ok := modelsDev[name]; ok {
			providers = append(providers, name)
		}
	}
	for name := range modelsDev {
		if !slices.Contains(preferredProviders, name) {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	providers = append(providers, rest...)
	seen := map[string]bool{}
	for _, provider := range providers {
		for name, m := range modelsDev[provider].Models {
			normalized := normalizeModel(name)
			if seen[normalized] || m.Cost == nil || m.Cost.Input == nil || m.Cost.Output == nil {
				continue
			}
			seen[normalized] = true
			out[normalized] = withCache(*m.Cost.Input, *m.Cost.Output, m.Cost.CacheRead, m.Cost.CacheWrite, 1_000_000)
		}
	}
	for model, m := range fast {
		if r, ok := out[model]; ok {
			r.fast = m
			out[model] = r
		}
	}
	return out
}

// withCache is a per-token rate from prices quoted per `per` tokens; a missing
// cache rate is priced at the input rate.
func withCache(input, output float64, cacheRead, cacheCreation *float64, per float64) rate {
	r := rate{input: input / per, output: output / per, cacheRead: input / per, cacheCreation: input / per, fast: 1}
	if cacheRead != nil {
		r.cacheRead = *cacheRead / per
	}
	if cacheCreation != nil {
		r.cacheCreation = *cacheCreation / per
	}
	return r
}

func fetchJSON(ctx context.Context, url string, into any) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(into) == nil
}
