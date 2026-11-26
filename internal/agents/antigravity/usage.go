package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentusage"
)

// Antigravity's limits are what agy itself answers to `-p /usage` with
// --output-format json: a read-only command that starts no turn, spends no
// quota and saves no conversation. agy asks Google with its own sign-in.

// Usage is the account's 5-hour and weekly limits per group of models, or
// last when agy can't answer.
func (p *Provider) Usage(ctx context.Context, last agentusage.Usage) agentusage.Usage {
	if p.deps.Root != "" {
		// A test host never reads the user's own account.
		return agentusage.Usage{}
	}
	windows, err := p.readUsage(ctx)
	if err != nil {
		if p.deps.Log != nil {
			p.deps.Log.Info("antigravity limits unavailable", "err", err)
		}
		if len(last.Windows) > 0 {
			return last
		}
		return agentusage.Usage{}
	}
	return agentusage.Usage{Available: true, CapturedAtMS: time.Now().UnixMilli(), Windows: windows}
}

func (p *Provider) readUsage(ctx context.Context) ([]agentusage.Window, error) {
	cmd := exec.CommandContext(ctx, executable(), "-p", "/usage", "--output-format", "json")
	cmd.Dir = os.TempDir()
	cmd.Env = append(agent.ChildEnv(), p.deps.Env...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return usageWindows(out, time.Now())
}

// usageAnswer is the parts of agy's answer the host shows.
type usageAnswer struct {
	Status  string `json:"status"`
	Command struct {
		Name string `json:"name"`
		Data struct {
			Groups []struct {
				Name    string        `json:"name"`
				Buckets []usageBucket `json:"buckets"`
			} `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

type usageBucket struct {
	ID                string   `json:"id"`
	Window            string   `json:"window"`
	RemainingFraction *float64 `json:"remaining_fraction"`
	ResetTime         string   `json:"reset_time"`
}

// usageWindows reads the answer's buckets as windows, the 5-hour one first in
// each group: Gemini's, and Claude and GPT's. An untouched window reports all
// of it left and no reset (1970), so it shows unused with no reset time. The
// most used window is the one that binds.
func usageWindows(raw []byte, now time.Time) ([]agentusage.Window, error) {
	var answer usageAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, err
	}
	if answer.Status != "SUCCESS" || answer.Command.Name != "usage" {
		return nil, errors.New("agy didn't answer /usage")
	}
	var out []agentusage.Window
	for _, group := range answer.Command.Data.Groups {
		scope := groupScope(group.Name)
		var windows []agentusage.Window
		for _, b := range group.Buckets {
			minutes, label := windowOf(b.Window)
			if b.RemainingFraction == nil || minutes == 0 {
				continue
			}
			w := agentusage.Window{
				ID: b.ID, Label: label + " · " + scope, Scope: scope, WindowMinutes: minutes,
				UsedPercent: math.Round(math.Max(0, math.Min(1, 1-*b.RemainingFraction))*1000) / 10,
			}
			if t, err := time.Parse(time.RFC3339, b.ResetTime); err == nil && t.After(now) {
				w.ResetsAt = t.Unix()
			}
			windows = append(windows, w)
		}
		// agy lists weekly first; the shorter window reads first, as Claude's.
		if len(windows) == 2 && windows[0].WindowMinutes > windows[1].WindowMinutes {
			windows[0], windows[1] = windows[1], windows[0]
		}
		out = append(out, windows...)
	}
	if len(out) == 0 {
		return nil, nil
	}
	most := 0
	for i, w := range out {
		if w.UsedPercent > out[most].UsedPercent {
			most = i
		}
	}
	out[most].IsActive = true
	return out, nil
}

// windowOf is a bucket's window as minutes and label.
func windowOf(window string) (int64, string) {
	switch window {
	case "5h":
		return 300, "5h"
	case "weekly":
		return 10080, "Weekly"
	}
	return 0, ""
}

// groupScope names a group of models shortly: "Gemini Models" is Gemini,
// "Claude and GPT models" is Claude & GPT.
func groupScope(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(strings.TrimSuffix(name, " models"), " Models")
	return strings.ReplaceAll(name, " and ", " & ")
}
