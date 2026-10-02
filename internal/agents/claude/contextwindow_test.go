package claude

import "testing"

func TestContextWindowFor(t *testing.T) {
	cases := map[string]int64{
		"claude-opus-5":   1_000_000,
		"claude-sonnet-5": 1_000_000,
		// The 5 generation is 1M and the 4 generation is 200k, so a prefix
		// match that stopped at "claude-sonnet" would answer for both.
		"claude-sonnet-4-5":              200_000,
		"claude-fable-5-1":               1_000_000,
		"us.anthropic.claude-opus-5":     1_000_000,
		"claude-opus-4-1-20250805":       200_000,
		"<synthetic>":                    0,
		"":                               0,
		"some-model-nobody-has-heard-of": 0,
	}
	for model, want := range cases {
		if got := contextWindowFor(model); got != want {
			t.Errorf("contextWindowFor(%q) = %d, want %d", model, got, want)
		}
	}
}
