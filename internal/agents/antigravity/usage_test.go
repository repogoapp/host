package antigravity

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestQuotaSummaryBecomesWindowsPerGroup(t *testing.T) {
	daily := `[{"displayName":"Gemini Models","buckets":[
		{"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","resetTime":"2026-11-29T05:24:12Z","remainingFraction":0.97769374},
		{"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","resetTime":"2026-11-22T10:24:12Z","remainingFraction":0}]},
		{"displayName":"Claude and GPT models","buckets":[
		{"bucketId":"3p-5h","displayName":"Five Hour Limit Remaining","window":"5h","resetTime":"2026-11-22T10:35:46Z","remainingFraction":1},
		{"bucketId":"off","window":"5h","remainingFraction":0.5,"disabled":true}]}]`
	prod := `[{"displayName":"Gemini Models","buckets":[
		{"bucketId":"gemini-5h","window":"5h","remainingFraction":1}]},
		{"displayName":"Gemini 4 Argon","buckets":[
		{"bucketId":"gemini-argon-wtu-weekly","displayName":"Gemini 4 Argon Weekly Limit","window":"weekly","remainingFraction":1}]}]`
	var a, b []quotaGroup
	if json.Unmarshal([]byte(daily), &a) != nil || json.Unmarshal([]byte(prod), &b) != nil {
		t.Fatal("fixture")
	}
	got := quotaWindows(a, b)
	if len(got) != 4 {
		t.Fatalf("windows %+v", got)
	}
	weekly, five, third, argon := got[0], got[1], got[2], got[3]
	if weekly.Label != "Weekly · Gemini" || weekly.WindowMinutes != 10080 || weekly.UsedPercent < 2.2 || weekly.UsedPercent > 2.3 ||
		weekly.ResetsAt != time.Date(2026, 11, 29, 5, 24, 12, 0, time.UTC).Unix() {
		t.Errorf("weekly %+v", weekly)
	}
	// The first answer's reading wins; a spent window is exceeded.
	if five.Label != "5h · Gemini" || five.UsedPercent != 100 || five.Severity != "exceeded" {
		t.Errorf("5h %+v", five)
	}
	// An untouched window's reset is only the rolling window, so none.
	if third.Label != "5h · Claude and GPT" || third.UsedPercent != 0 || third.ResetsAt != 0 {
		t.Errorf("3p %+v", third)
	}
	if argon.Label != "Weekly · Gemini 4 Argon" || argon.Scope != "Gemini 4 Argon" {
		t.Errorf("argon %+v", argon)
	}
}

func TestSavedSignInsAreRead(t *testing.T) {
	token := `{"token":{"access_token":"a","refresh_token":"r","expiry":"2026-11-22T01:00:12.315222-05:00"}}`
	for _, raw := range []string{token, "go-keyring-base64:" + base64.StdEncoding.EncodeToString([]byte(token)), `{"access_token":"a","expiry":"2026-11-22T06:00:12Z"}`} {
		got, err := parseToken(raw)
		if err != nil || got.AccessToken != "a" || got.Expiry.IsZero() {
			t.Errorf("%q: %+v %v", raw[:20], got, err)
		}
	}
	if _, err := parseToken(`{"token":{}}`); err == nil {
		t.Error("an empty token was read")
	}
}

func TestThePlanComesFromTheSession(t *testing.T) {
	p := &Provider{limits: &limits{}}
	p.notePlan(json.RawMessage(`{"antigravity":{"tier":{"name":"Google AI Pro","id":"g1-pro-tier"}}}`))
	p.notePlan(json.RawMessage(`{}`))
	if p.limits.plan != "Google AI Pro" {
		t.Fatalf("plan %q", p.limits.plan)
	}
}
