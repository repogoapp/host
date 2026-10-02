package cursor

import (
	"encoding/json"
	"testing"
	"time"
)

// Connect writes 64-bit ints as strings and leaves zeros out: a Pro plan with
// nothing spent on demand has no individualUsed at all.
func TestUsageFromTheDashboard(t *testing.T) {
	var period cursorPeriod
	var plan cursorPlan
	var credits cursorCredits
	for raw, into := range map[string]any{
		`{"billingCycleEnd":"1792102444000","planUsage":{"totalPercentUsed":4.43},"spendLimitUsage":{"individualLimit":10000,"individualRemaining":10000}}`: &period,
		`{"planInfo":{"planName":"Pro"}}`: &plan,
		`{"hasCreditGrants":true,"totalCents":"2000","usedCents":"1900"}`: &credits,
	} {
		if err := json.Unmarshal([]byte(raw), into); err != nil {
			t.Fatal(err)
		}
	}

	usage := parseCursorUsage(period, plan, credits, time.Now())
	if !usage.Available || usage.Plan != "Pro" || len(usage.Windows) != 3 {
		t.Fatalf("usage = %+v", usage)
	}
	demand, grant, included := usage.Windows[0], usage.Windows[1], usage.Windows[2]
	if demand.ID != "on_demand" || !demand.IsActive || demand.Unit != "currency" || demand.UsedValue != 0 ||
		demand.LimitValue != 10000 || demand.UsedPercent != 0 || demand.ResetsAt != 1792102444 {
		t.Fatalf("on-demand = %+v", demand)
	}
	if grant.ID != "credits" || grant.UsedPercent != 95 || grant.Severity != "warning" || grant.IsActive {
		t.Fatalf("credits = %+v", grant)
	}
	if included.ID != "included" || included.Unit != "percent" || included.UsedPercent != 4.43 || included.ResetsAt != 1792102444 {
		t.Fatalf("included = %+v", included)
	}
}

// An account with no spend limit and no grants still reads, from the included
// allowance alone.
func TestUsageWithoutSpendLimit(t *testing.T) {
	var period cursorPeriod
	if err := json.Unmarshal([]byte(`{"planUsage":{"totalPercentUsed":120}}`), &period); err != nil {
		t.Fatal(err)
	}
	usage := parseCursorUsage(period, cursorPlan{}, cursorCredits{}, time.Now())
	if len(usage.Windows) != 1 || usage.Windows[0].UsedPercent != 100 || usage.Windows[0].ResetsAt != 0 {
		t.Fatalf("usage = %+v", usage)
	}
}
