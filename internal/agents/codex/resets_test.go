package codex

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The shape `codex app-server` 0.156 answers account/rateLimits/read with.
const codexAccountRead = `{
  "ordinaryUsageAllowed": true,
  "rateLimits": {
    "limitId": "codex",
    "primary": {"usedPercent": 75, "windowDurationMins": 10080, "resetsAt": 1790441020},
    "secondary": null,
    "planType": "prolite",
    "rateLimitReachedType": null
  },
  "rateLimitResetCredits": {
    "availableCount": 3,
    "credits": [
      {"id": "RateLimitResetCredit_a", "resetType": "codexRateLimits", "status": "available",
       "grantedAt": 1788488190, "expiresAt": 1791080190, "title": "Full reset", "description": "Thanks"},
      {"id": "RateLimitResetCredit_b", "resetType": "codexRateLimits", "status": "redeemed",
       "grantedAt": 1788582145, "expiresAt": null, "title": null, "description": null}
    ]
  }
}`

func TestCodexAccountUsageCarriesResetCredits(t *testing.T) {
	usage, ok := parseCodexAccountUsage(json.RawMessage(codexAccountRead), time.Unix(1790000000, 0))
	if !ok {
		t.Fatal("parse failed")
	}
	if len(usage.Windows) != 1 || usage.Windows[0].Label != "Weekly" || usage.Windows[0].ResetsAt != 1790441020 {
		t.Fatalf("windows = %+v", usage.Windows)
	}
	if usage.Resets == nil || usage.Resets.Available != 3 || len(usage.Resets.Credits) != 2 {
		t.Fatalf("resets = %+v", usage.Resets)
	}
	first, second := usage.Resets.Credits[0], usage.Resets.Credits[1]
	if first.ID != "RateLimitResetCredit_a" || first.Title != "Full reset" || first.Status != "available" ||
		first.ExpiresAt != 1791080190 || first.ResetType != "codexRateLimits" {
		t.Fatalf("first credit = %+v", first)
	}
	// Nulls decode to empty, never fail the whole reading.
	if second.Title != "" || second.ExpiresAt != 0 || second.Status != "redeemed" {
		t.Fatalf("second credit = %+v", second)
	}
}

func TestCodexAccountUsageWithoutResetCredits(t *testing.T) {
	raw := `{"rateLimits":{"primary":{"usedPercent":10,"windowDurationMins":300,"resetsAt":1}}}`
	usage, ok := parseCodexAccountUsage(json.RawMessage(raw), time.Now())
	if !ok || usage.Resets != nil {
		t.Fatalf("ok=%v resets=%+v", ok, usage.Resets)
	}
	// Absent from the wire, so a phone reads "no resets" rather than "zero".
	data, _ := json.Marshal(usage)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	if _, present := decoded["resets"]; present {
		t.Fatalf("resets serialized: %s", data)
	}
}

func TestCodexAccountUsageCountOnly(t *testing.T) {
	raw := `{"rateLimits":{"primary":{"usedPercent":10,"windowDurationMins":300}},"rateLimitResetCredits":{"availableCount":2,"credits":null}}`
	usage, _ := parseCodexAccountUsage(json.RawMessage(raw), time.Now())
	if usage.Resets == nil || usage.Resets.Available != 2 || usage.Resets.Credits == nil || len(usage.Resets.Credits) != 0 {
		t.Fatalf("resets = %+v", usage.Resets)
	}
}

func TestParseResetOutcome(t *testing.T) {
	for raw, want := range map[string]string{
		`{"outcome":"reset"}`:           resetDone,
		`{"outcome":"nothingToReset"}`:  resetNothingToReset,
		`{"outcome":"noCredit"}`:        resetNoCredit,
		`{"outcome":"alreadyRedeemed"}`: resetAlreadyRedeemed,
		`{"outcome":"somethingNew"}`:    "somethingNew",
	} {
		got, err := parseResetOutcome(json.RawMessage(raw))
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{`{}`, `null`, `"reset"`} {
		if _, err := parseResetOutcome(json.RawMessage(raw)); !errors.Is(err, ErrResetFailed) {
			t.Errorf("%s: err = %v, want ErrResetFailed", raw, err)
		}
	}
}
