package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/repogo/host/internal/agentusage"
	"github.com/repogo/host/internal/clitool"
)

// dashboardURL is Cursor's undocumented Connect-RPC dashboard service, the one
// its website reads usage from.
const dashboardURL = "https://api2.cursor.sh/aiserver.v1.DashboardService/"

// A billing month, so spend windows sort beside each other on the phone.
const billingMonthMinutes = 43200

// Usage reads the billing period from Cursor's dashboard API with the CLI's
// token. Cursor bills by spend, not time windows, so its windows are dollars.
func (p *Provider) Usage(ctx context.Context, last agentusage.Usage) agentusage.Usage {
	token := p.accessToken(ctx)
	if token == "" {
		return agentusage.Usage{Detail: "no Cursor login (sign in with the Cursor CLI)"}
	}
	_, _, account := p.authenticated(ctx, p.executable())

	var period cursorPeriod
	if !dashboardCall(ctx, token, "GetCurrentPeriodUsage", &period) {
		if len(last.Windows) > 0 {
			last.Detail = "Cursor usage endpoint unavailable; showing last known windows"
			return last
		}
		return agentusage.Usage{Account: account, Detail: "Cursor usage endpoint unavailable"}
	}
	var plan cursorPlan
	dashboardCall(ctx, token, "GetPlanInfo", &plan)
	var credits cursorCredits
	dashboardCall(ctx, token, "GetCreditGrantsBalance", &credits)

	usage := parseCursorUsage(period, plan, credits, time.Now())
	usage.Account = account
	return usage
}

// cursorInt reads an int64 as Connect's JSON writes it: a string for 64-bit
// fields, a number for 32-bit ones.
type cursorInt int64

func (n *cursorInt) UnmarshalJSON(raw []byte) error {
	value, err := strconv.ParseInt(strings.Trim(string(raw), `"`), 10, 64)
	if err != nil {
		return err
	}
	*n = cursorInt(value)
	return nil
}

// Zero values are left out of Connect's JSON, so every field here may be absent.
type cursorPeriod struct {
	BillingCycleEnd cursorInt `json:"billingCycleEnd"`
	PlanUsage       *struct {
		TotalPercentUsed *float64 `json:"totalPercentUsed"`
	} `json:"planUsage"`
	SpendLimitUsage *struct {
		IndividualUsed  cursorInt `json:"individualUsed"`
		IndividualLimit cursorInt `json:"individualLimit"`
	} `json:"spendLimitUsage"`
}

type cursorPlan struct {
	PlanInfo struct {
		PlanName string `json:"planName"`
	} `json:"planInfo"`
}

type cursorCredits struct {
	HasCreditGrants bool      `json:"hasCreditGrants"`
	TotalCents      cursorInt `json:"totalCents"`
	UsedCents       cursorInt `json:"usedCents"`
}

// parseCursorUsage builds the windows: the on-demand spend cap, which is what
// blocks a turn and so is the ring, then promo credits, then the included
// allowance, which bonus usage runs past without blocking.
func parseCursorUsage(period cursorPeriod, plan cursorPlan, credits cursorCredits, now time.Time) agentusage.Usage {
	usage := agentusage.Usage{
		Available:    true,
		Plan:         plan.PlanInfo.PlanName,
		CapturedAtMS: now.UnixMilli(),
		Detail:       "live from api2.cursor.sh (undocumented dashboard API)",
		Windows:      []agentusage.Window{},
	}
	resetsAt := int64(period.BillingCycleEnd) / 1000

	if spend := period.SpendLimitUsage; spend != nil {
		window := spendWindow("on_demand", "On-demand", int64(spend.IndividualUsed), int64(spend.IndividualLimit))
		window.WindowMinutes = billingMonthMinutes
		window.ResetsAt = resetsAt
		window.IsActive = true
		usage.Windows = append(usage.Windows, window)
	}
	if credits.HasCreditGrants && credits.TotalCents > 0 {
		usage.Windows = append(usage.Windows, spendWindow("credits", "Credits", int64(credits.UsedCents), int64(credits.TotalCents)))
	}
	if period.PlanUsage != nil && period.PlanUsage.TotalPercentUsed != nil {
		usage.Windows = append(usage.Windows, agentusage.Window{
			ID:            "included",
			Label:         "Included",
			Unit:          "percent",
			UsedPercent:   min(*period.PlanUsage.TotalPercentUsed, 100),
			WindowMinutes: billingMonthMinutes,
			ResetsAt:      resetsAt,
			Severity:      "normal",
		})
	}
	return usage
}

// spendWindow is cents used of cents allowed, warning from 90%.
func spendWindow(id, label string, used, limit int64) agentusage.Window {
	window := agentusage.Window{
		ID:         id,
		Label:      label,
		Unit:       "currency",
		Currency:   "USD",
		UsedValue:  used,
		LimitValue: limit,
		Severity:   "normal",
	}
	if limit > 0 {
		window.UsedPercent = min(float64(used)/float64(limit)*100, 100)
	}
	if window.UsedPercent >= 90 {
		window.Severity = "warning"
	}
	return window
}

// dashboardCall posts an empty Connect request and decodes the reply; any
// failure, a 401 from an expired token included, is false.
func dashboardCall(ctx context.Context, token, method string, into any) bool {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dashboardURL+method, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(res.Body).Decode(into) == nil
}

// accessToken is the token the CLI keeps fresh: the macOS Keychain item it
// writes, then the auth.json it writes elsewhere. The host never refreshes it.
func (p *Provider) accessToken(ctx context.Context) string {
	if runtime.GOOS == "darwin" && p.deps.Root == "" {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(probe, "security",
			"find-generic-password", "-s", "cursor-access-token", "-a", "cursor-user", "-w").Output()
		if token := strings.TrimSpace(string(out)); err == nil && token != "" {
			return token
		}
	}
	home := p.deps.Root
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	for _, path := range []string{filepath.Join(home, ".config", "cursor", "auth.json"), filepath.Join(p.home, "auth.json")} {
		var file struct {
			AccessToken string `json:"accessToken"`
		}
		if clitool.ReadJSON(path, &file) && file.AccessToken != "" {
			return file.AccessToken
		}
	}
	return ""
}
