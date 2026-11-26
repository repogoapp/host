package antigravity

import (
	"testing"
	"time"
)

// agy 1.5.18's answer to `-p /usage --output-format json`, trimmed.
const usageAnswerJSON = `{"conversation_id":"","status":"SUCCESS","num_turns":0,"command":{"name":"usage","data":{"groups":[
	{"name":"Gemini Models","buckets":[
		{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.9952133297920227,"reset_time":"2026-12-03T18:26:17Z"},
		{"id":"gemini-5h","window":"5h","remaining_fraction":0.9799333214759827,"reset_time":"2026-11-26T23:26:17Z"}]},
	{"name":"Claude and GPT models","buckets":[
		{"id":"3p-weekly","window":"weekly","remaining_fraction":1,"reset_time":"1970-01-01T00:00:00Z"},
		{"id":"3p-5h","window":"5h","remaining_fraction":1,"reset_time":"1970-01-01T00:00:00Z"}]}]}}}`

func TestUsageWindows(t *testing.T) {
	now := time.Date(2026, 11, 26, 19, 0, 0, 0, time.UTC)
	got, err := usageWindows([]byte(usageAnswerJSON), now)
	if err != nil {
		t.Fatal(err)
	}
	// The unused Claude & GPT group shows with nothing used and no reset.
	if len(got) != 4 || got[2].ID != "3p-5h" || got[2].Label != "5h · Claude & GPT" || got[2].UsedPercent != 0 || got[2].ResetsAt != 0 ||
		got[3].ID != "3p-weekly" {
		t.Fatalf("windows = %+v", got)
	}
	if got[0].ID != "gemini-5h" || got[0].Label != "5h · Gemini" || got[0].Scope != "Gemini" || got[0].WindowMinutes != 300 ||
		got[0].UsedPercent != 2 || got[0].ResetsAt != 1795735577 || !got[0].IsActive {
		t.Fatalf("5h = %+v", got[0])
	}
	if got[1].ID != "gemini-weekly" || got[1].Label != "Weekly · Gemini" || got[1].WindowMinutes != 10080 || got[1].UsedPercent != 0.5 || got[1].IsActive {
		t.Fatalf("weekly = %+v", got[1])
	}
}

// A used third-party group binds when it is the most used.
func TestUsageWindowsThirdParty(t *testing.T) {
	answer := `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[
		{"name":"Gemini Models","buckets":[{"id":"gemini-5h","window":"5h","remaining_fraction":1,"reset_time":"1970-01-01T00:00:00Z"}]},
		{"name":"Claude and GPT models","buckets":[{"id":"3p-5h","window":"5h","remaining_fraction":0.4,"reset_time":"2026-11-27T00:00:00Z"}]}]}}}`
	got, err := usageWindows([]byte(answer), time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UsedPercent != 0 || got[0].ResetsAt != 0 || got[1].Label != "5h · Claude & GPT" || got[1].UsedPercent != 60 || !got[1].IsActive {
		t.Fatalf("windows = %+v", got)
	}
}

func TestUsageWindowsRefusal(t *testing.T) {
	if _, err := usageWindows([]byte(`{"status":"ERROR"}`), time.Now()); err == nil {
		t.Fatal("a failed answer gave windows")
	}
	if _, err := usageWindows([]byte(`not json`), time.Now()); err == nil {
		t.Fatal("bad JSON gave windows")
	}
}
