package claude

import "strings"

// modelContextWindows maps a model name to its input window, because Claude Code
// records what a request consumed but never the window it had. Embedded rather
// than fetched; an unknown model returns 0 and the client shows a raw count.
var modelContextWindows = map[string]int64{
	// 1M-token generation.
	"claude-opus-5":    1_000_000,
	"claude-sonnet-5":  1_000_000,
	"claude-fable-5":   1_000_000,
	"claude-fable-5-1": 1_000_000,
	"claude-haiku-5":   1_000_000,

	// 200k generation.
	"claude-sonnet-4-5": 200_000,
	"claude-opus-4-1":   200_000,
	"claude-opus-4":     200_000,
	"claude-sonnet-4":   200_000,
	"claude-haiku-4-5":  200_000,
	"claude-3-7-sonnet": 200_000,
	"claude-3-5-sonnet": 200_000,
	"claude-3-5-haiku":  200_000,
}

// contextWindowFor matches on a normalized prefix because wire names carry
// dated suffixes and vendor prefixes. The longest key wins, so `claude-opus-4`
// does not answer for `claude-opus-4-1`.
func contextWindowFor(model string) int64 {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return 0
	}
	// Strip provider routing prefixes: bedrock and vertex spell the same model
	// `us.anthropic.claude-opus-5`, and the last segment is the model itself.
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}

	var best int64
	var bestLen int
	for key, window := range modelContextWindows {
		if strings.HasPrefix(name, key) && len(key) > bestLen {
			best, bestLen = window, len(key)
		}
	}
	return best
}
