package codex

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/repogo/host/internal/agent"
)

// A cell's output as Codex stores it: the header, then the JSON each awaited
// exec_command printed, the last one cut short by truncation.
const twoCommandOutput = "Script completed\nWall time 1.25 seconds\nOutput:\n" +
	`{"chunk_id":"a1","wall_time_seconds":0.5,"exit_code":0,"original_token_count":3,"output":"ok\n"}` + "\n" +
	`{"chunk_id":"b2","wall_time_seconds":0.25,"exit_code":1,"original_token_count":4,"output":"fail\n"}` + "\n" +
	`{"chunk_id":"c3","wall_time_seconds":0.1,"exit_code":0,"output":"cut`

func TestCellResultReadsEachCommand(t *testing.T) {
	script := `const a = await tools.exec_command({cmd: "go build"}); console.log(JSON.stringify(a));
const b = await tools.exec_command({cmd: "go test"}); console.log(JSON.stringify(b));`
	got := (&Provider{}).Resolve(agent.ToolCall{Name: "exec", Input: json.RawMessage(cellInput(t, script)), Output: twoCommandOutput})
	zero, one := 0, 1
	want := &agent.ToolResult{
		ExitCode:   &one,
		DurationMS: 1250,
		Commands: []agent.ToolCommand{
			{Command: "go build", ExitCode: &zero, DurationMS: 500, Output: "ok\n"},
			{Command: "go test", ExitCode: &one, DurationMS: 250, Output: "fail\n"},
		},
	}
	if !reflect.DeepEqual(got.Result, want) {
		t.Errorf("got %+v, want %+v", got.Result, want)
	}
}

// One command gives the cell's exit code and time, with no list to repeat it.
func TestCellResultOneCommand(t *testing.T) {
	output := "Script completed\nWall time 0.1 seconds\nOutput:\n" +
		`{"chunk_id":"a1","wall_time_seconds":0.05,"exit_code":0,"output":"ok"}`
	got := cellResult([]cellCall{{Name: "exec_command", Input: map[string]any{"cmd": "ls"}}}, output)
	zero := 0
	if want := (&agent.ToolResult{ExitCode: &zero, DurationMS: 100}); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got := cellResult(nil, "plain text"); got != nil {
		t.Errorf("plain output: got %+v, want nil", got)
	}
}
