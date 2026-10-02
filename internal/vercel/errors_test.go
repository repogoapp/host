package vercel

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// Errors are counted by status, the busiest path wins, and At is the first.
func TestReadRuntimeErrors(t *testing.T) {
	run := func(_ context.Context, args []string) ([]byte, error) {
		want := []string{"logs", "--project", "p1", "--scope", "team_a", "--environment", "production",
			"--status-code", "5xx", "--since", "15m", "--json", "--non-interactive"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args: %v", args)
		}
		return []byte(`Fetching logs...
{"requestPath":"/api/checkout","responseStatusCode":503,"timestamp":3000}
{"requestPath":"/api/login","responseStatusCode":500,"timestamp":1000}
{"requestPath":"/api/checkout","responseStatusCode":502,"timestamp":2000}
{"requestPath":"/ok","responseStatusCode":200,"timestamp":500}
`), nil
	}
	got, err := readErrorsWith(t.Context(), "team_a", "p1", run)
	want := RuntimeErrors{Count: 3, Path: "/api/checkout", At: time.UnixMilli(1000)}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestReadRuntimeErrorsNone(t *testing.T) {
	got, err := readErrorsWith(t.Context(), "team_a", "p1", func(context.Context, []string) ([]byte, error) { return nil, nil })
	if err != nil || got != (RuntimeErrors{}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}
