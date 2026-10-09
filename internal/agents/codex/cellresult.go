package codex

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/toollabel"
)

// cellCommand is what a cell prints for each `tools.exec_command` it awaits.
type cellCommand struct {
	WallTimeSeconds float64 `json:"wall_time_seconds"`
	ExitCode        *int    `json:"exit_code"`
	Output          string  `json:"output"`
}

// cellResult reads a cell's wall time from its header and each command's
// printed JSON, matched in order to its exec_command calls. One command gives
// its exit code alone; Commands is for two or more.
func cellResult(calls []cellCall, output string) *agent.ToolResult {
	r := agent.ToolResult{DurationMS: wallTimeMS(output)}
	var commands []string
	for _, c := range calls {
		if c.Name == "exec_command" {
			args, _ := c.Input.(map[string]any)
			commands = append(commands, toollabel.ShellCommand(args))
		}
	}
	ran := cellCommands(output)
	for i, c := range ran {
		cmd := agent.ToolCommand{ExitCode: c.ExitCode, DurationMS: int64(c.WallTimeSeconds * 1000), Output: c.Output}
		if i < len(commands) {
			cmd.Command = commands[i]
		}
		r.Commands = append(r.Commands, cmd)
		if r.ExitCode == nil || *r.ExitCode == 0 {
			r.ExitCode = c.ExitCode
		}
	}
	if len(r.Commands) < 2 {
		r.Commands = nil
	}
	if r.DurationMS == 0 && r.ExitCode == nil {
		return nil
	}
	return &r
}

// cellCommands are the command results a cell printed, each an object that
// starts with its chunk_id. One cut short by Codex's truncation is skipped.
func cellCommands(output string) []cellCommand {
	const marker = `{"chunk_id"`
	var out []cellCommand
	for rest := output; ; {
		i := strings.Index(rest, marker)
		if i < 0 {
			return out
		}
		rest = rest[i:]
		var c cellCommand
		if json.NewDecoder(strings.NewReader(rest)).Decode(&c) == nil && c.ExitCode != nil {
			out = append(out, c)
		}
		rest = rest[len(marker):]
	}
}

// wallTimeMS is the "Wall time N seconds" line of a cell's header, in ms.
func wallTimeMS(output string) int64 {
	header, _, _ := strings.Cut(output, "Output:")
	for line := range strings.SplitSeq(header, "\n") {
		text, ok := strings.CutPrefix(line, "Wall time ")
		if !ok {
			continue
		}
		seconds, err := strconv.ParseFloat(strings.TrimSuffix(text, " seconds"), 64)
		if err == nil {
			return int64(seconds * 1000)
		}
	}
	return 0
}
