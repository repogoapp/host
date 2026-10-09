package claude

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"

	"github.com/repogo/host/internal/agent"
)

// claudeToolUseResult is the part of the CLI's `toolUseResult`, written beside
// a tool_result, that a tool page draws. Which fields are set tells the tools
// apart: Interrupted is only on a shell run.
type claudeToolUseResult struct {
	Type            string            `json:"type"`
	Stdout          string            `json:"stdout"`
	Stderr          string            `json:"stderr"`
	Interrupted     *bool             `json:"interrupted"`
	TimedOutAfterMs int64             `json:"timedOutAfterMs"`
	Code            int               `json:"code"`
	DurationMs      float64           `json:"durationMs"`
	DurationSeconds float64           `json:"durationSeconds"`
	Results         []json.RawMessage `json:"results"`
}

// toolResult reads a result's `toolUseResult`. A shell run that wrote to
// stderr has its output as stdout alone, the stderr kept apart. A failed run
// has a string there, so its exit code is read from the text.
func toolResult(raw json.RawMessage, output string, isError bool) (string, *agent.ToolResult) {
	r := agent.ToolResult{}
	if isError {
		r.ExitCode = exitCode(output)
	}
	var u claudeToolUseResult
	if len(raw) > 0 && raw[0] == '{' && sonic.Unmarshal(raw, &u) == nil {
		if u.Interrupted != nil {
			r.Interrupted = *u.Interrupted
			r.TimedOut = u.TimedOutAfterMs > 0
			if strings.TrimSpace(u.Stderr) != "" {
				r.Stderr = u.Stderr
				output = u.Stdout
			}
		}
		r.Created = u.Type == "create"
		r.HTTPStatus = u.Code
		r.DurationMS = int64(u.DurationMs)
		if u.DurationSeconds > 0 {
			r.DurationMS = int64(u.DurationSeconds * 1000)
		}
		r.URLs = searchURLs(u.Results)
	}
	if r.ExitCode == nil && r.DurationMS == 0 && !r.Interrupted && !r.TimedOut && r.Stderr == "" &&
		!r.Created && len(r.URLs) == 0 && r.HTTPStatus == 0 {
		return output, nil
	}
	return output, &r
}

// exitCode is N from a failed shell run's "Exit code N" first line.
func exitCode(output string) *int {
	line, _, _ := strings.Cut(output, "\n")
	digits, ok := strings.CutPrefix(line, "Exit code ")
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return nil
	}
	return &n
}

// searchURLs are the pages a web search found. Its results mix summary
// strings with blocks of links; only the links are read.
func searchURLs(results []json.RawMessage) []agent.ToolURL {
	var out []agent.ToolURL
	for _, raw := range results {
		var block struct {
			Content []agent.ToolURL `json:"content"`
		}
		if len(raw) == 0 || raw[0] != '{' || sonic.Unmarshal(raw, &block) != nil {
			continue
		}
		for _, u := range block.Content {
			if u.URL != "" {
				out = append(out, u)
			}
		}
	}
	return out
}
