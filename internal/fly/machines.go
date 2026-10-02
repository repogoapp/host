package fly

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
)

// MachineSummary is configured capacity across all non-destroyed machines,
// not utilization. A failed read keeps the deployment card usable.
type MachineSummary struct {
	Count    int      `json:"count"`
	Running  int      `json:"running"`
	State    string   `json:"state"`
	Regions  []string `json:"regions" wire:"array"`
	CPUs     int      `json:"cpus"`
	MemoryMB int      `json:"memory_mb"`
	Error    string   `json:"error"`
}

func readMachines(ctx context.Context, app string, run func(context.Context, []string) ([]byte, error)) *MachineSummary {
	result := &MachineSummary{Regions: []string{}}
	out, err := run(ctx, []string{"machine", "list", "-a", app, "--json"})
	if err != nil {
		result.Error, _, _ = strings.Cut(err.Error(), "\n")
		if len(result.Error) > 200 {
			result.Error = result.Error[:200] + "…"
		}
		return result
	}
	var machines []struct {
		State  string `json:"state"`
		Region string `json:"region"`
		Config struct {
			Guest *struct {
				CPUs     int `json:"cpus"`
				MemoryMB int `json:"memory_mb"`
			} `json:"guest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(out, &machines); err != nil {
		result.Error = "Couldn’t read machine details"
		return result
	}
	complete := true
	for _, machine := range machines {
		if machine.State == "destroyed" {
			continue
		}
		result.Count++
		state := machine.State
		if state == "" {
			state = "unknown"
		}
		if result.Count == 1 {
			result.State = state
		} else if result.State != state {
			result.State = "mixed"
		}
		if state == "started" {
			result.Running++
		}
		if machine.Region != "" && !slices.Contains(result.Regions, machine.Region) {
			result.Regions = append(result.Regions, machine.Region)
		}
		if guest := machine.Config.Guest; guest != nil && guest.CPUs > 0 && guest.MemoryMB > 0 {
			result.CPUs += guest.CPUs
			result.MemoryMB += guest.MemoryMB
		} else {
			complete = false
		}
	}
	// Partial capacity would understate an app's configured resources.
	if !complete {
		result.CPUs, result.MemoryMB = 0, 0
	}
	slices.Sort(result.Regions)
	return result
}
