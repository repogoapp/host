package vercel

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/repogo/host/internal/errkind"
)

type envCall struct {
	args  []string
	stdin []byte
}

// fakeEnvCLI answers each call in turn and records what it was asked.
func fakeEnvCLI(t *testing.T, replies ...string) (inputRunner, *[]envCall) {
	t.Helper()
	calls := &[]envCall{}
	return func(_ context.Context, args []string, stdin []byte) ([]byte, error) {
		if len(*calls) == len(replies) {
			t.Fatalf("unexpected call %v", args)
		}
		*calls = append(*calls, envCall{args, stdin})
		return []byte(replies[len(*calls)-1]), nil
	}, calls
}

// The list leaves out system variables and never carries a value; an older
// record's single target becomes a list.
func TestListEnv(t *testing.T) {
	run, calls := fakeEnvCLI(t, `{"envs":[
		{"id":"e1","key":"API_URL","value":"https://x","type":"encrypted","target":["production","preview"],"gitBranch":null},
		{"id":"e2","key":"TOKEN","value":"","type":"sensitive","target":"preview","gitBranch":"feat"},
		{"id":"e3","key":"VERCEL_URL","value":"","type":"system","target":["production"]}],"hiddenProductionEnvCount":0}`)
	got, err := listEnvWith(t.Context(), "team_a", "prj_1", run)
	want := []EnvVar{
		{ID: "e1", Key: "API_URL", Targets: []string{"production", "preview"}},
		{ID: "e2", Key: "TOKEN", Targets: []string{"preview"}, GitBranch: "feat", Sensitive: true},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if want := []string{"api", "/v10/projects/prj_1/env?teamId=team_a", "-X", "GET", "--raw", "--non-interactive"}; !reflect.DeepEqual((*calls)[0].args, want) {
		t.Fatalf("args %v", (*calls)[0].args)
	}
}

func TestEnvValueRefusesSensitive(t *testing.T) {
	run, _ := fakeEnvCLI(t, `{"id":"e1","key":"API_URL","value":"https://x","type":"encrypted"}`)
	if got, err := envValueWith(t.Context(), "team_a", "prj_1", "e1", run); err != nil || got != "https://x" {
		t.Fatalf("got %q, %v", got, err)
	}
	run, _ = fakeEnvCLI(t, `{"id":"e2","key":"TOKEN","value":"","type":"sensitive"}`)
	if _, err := envValueWith(t.Context(), "team_a", "prj_1", "e2", run); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("err %v", err)
	}
}

// A new variable is a POST with its value on stdin, not in the arguments.
func TestSetEnvCreates(t *testing.T) {
	run, calls := fakeEnvCLI(t, `{"created":{"id":"e9","key":"API_URL","type":"sensitive","target":["production"]},"failed":[]}`)
	got, err := setEnvWith(t.Context(), EnvWrite{TeamID: "team_a", ProjectID: "prj_1", Key: "API_URL", Value: "s3cret",
		Targets: []string{"production"}, Sensitive: true}, run)
	if err != nil || !reflect.DeepEqual(got, EnvVar{ID: "e9", Key: "API_URL", Targets: []string{"production"}, Sensitive: true}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	call := (*calls)[0]
	if want := []string{"api", "/v10/projects/prj_1/env?teamId=team_a", "-X", "POST", "--raw", "--non-interactive", "--input", "-"}; !reflect.DeepEqual(call.args, want) {
		t.Fatalf("args %v", call.args)
	}
	var body map[string]any
	if err := json.Unmarshal(call.stdin, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"key": "API_URL", "value": "s3cret", "type": "sensitive", "target": []any{"production"}}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body %v", body)
	}
}

// A change is a PATCH of that row: an empty value keeps the current one, and
// new targets without a branch clear the old branch.
func TestSetEnvChanges(t *testing.T) {
	run, calls := fakeEnvCLI(t, `{"id":"e1","key":"API_URL","type":"encrypted","target":["preview","development"]}`)
	_, err := setEnvWith(t.Context(), EnvWrite{TeamID: "team_a", ProjectID: "prj_1", ID: "e1", Key: "API_URL",
		Targets: []string{"preview", "development"}}, run)
	if err != nil {
		t.Fatal(err)
	}
	call := (*calls)[0]
	if call.args[1] != "/v9/projects/prj_1/env/e1?teamId=team_a" || call.args[3] != "PATCH" {
		t.Fatalf("args %v", call.args)
	}
	var body map[string]any
	if err := json.Unmarshal(call.stdin, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"key": "API_URL", "target": []any{"preview", "development"}, "gitBranch": nil}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body %v", body)
	}
}

func TestSetEnvRefusesWhatVercelWould(t *testing.T) {
	run := func(context.Context, []string, []byte) ([]byte, error) { t.Fatal("ran the CLI"); return nil, nil }
	for _, w := range []EnvWrite{
		{TeamID: "team_a", ProjectID: "prj_1", Key: "A", Value: "v"},
		{TeamID: "team_a", ProjectID: "prj_1", Key: "A", Value: "v", Targets: []string{"staging"}},
		{TeamID: "team_a", ProjectID: "prj_1", Key: "A", Value: "v", Targets: []string{"development"}, Sensitive: true},
		{TeamID: "team_a", ProjectID: "prj_1", Key: "A", Value: "v", Targets: []string{"production"}, GitBranch: "feat"},
		{ProjectID: "prj_1", Key: "A", Value: "v", Targets: []string{"production"}},
	} {
		if _, err := setEnvWith(t.Context(), w, run); !errors.Is(err, errkind.ErrInvalid) {
			t.Fatalf("%+v: err %v", w, err)
		}
	}
}

// Vercel answers a POST for several targets with a list.
func TestDecodeWrittenList(t *testing.T) {
	got, err := decodeWritten([]byte(`{"created":[{"id":"e1","key":"A","type":"encrypted","target":["preview"]}]}`))
	if err != nil || got.ID != "e1" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestRemoveEnv(t *testing.T) {
	run, calls := fakeEnvCLI(t, `{}`)
	if err := removeEnvWith(t.Context(), "team_a", "prj_1", "e1", run); err != nil {
		t.Fatal(err)
	}
	want := []string{"api", "/v9/projects/prj_1/env/e1?teamId=team_a", "-X", "DELETE", "--raw", "--non-interactive", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual((*calls)[0].args, want) {
		t.Fatalf("args %v", (*calls)[0].args)
	}
}

// Redeploying rebuilds the newest production deployment, without waiting.
func TestRedeployProduction(t *testing.T) {
	run, calls := fakeEnvCLI(t, `{"deployments":[{"uid":"dpl_1","url":"web.vercel.app","target":"production","readyState":"READY","created":1}],"pagination":{"next":null}}`, `https://web.vercel.app`)
	if err := redeployWith(t.Context(), "team_a", "prj_1", run); err != nil {
		t.Fatal(err)
	}
	want := []string{"redeploy", "dpl_1", "--target", "production", "--no-wait", "--scope", "team_a", "--non-interactive"}
	if !reflect.DeepEqual((*calls)[1].args, want) {
		t.Fatalf("args %v", (*calls)[1].args)
	}
	run, _ = fakeEnvCLI(t, `{"deployments":[],"pagination":{"next":null}}`)
	if err := redeployWith(t.Context(), "team_a", "prj_1", run); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("err %v", err)
	}
}
