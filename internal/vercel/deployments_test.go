package vercel

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/repogo/host/internal/errkind"
)

// A page keeps only what a row shows, and its cursor is Vercel's next.
func TestReadDeployments(t *testing.T) {
	run := func(_ context.Context, args []string) ([]byte, error) {
		want := []string{"api", "/v6/deployments?limit=2&projectId=prj_1&state=ERROR&target=production&teamId=team_a&until=900", "--raw", "--non-interactive"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args:\n%v\nwant\n%v", args, want)
		}
		return []byte(`{"deployments":[
			{"uid":"d1","url":"web-1.vercel.app","target":"production","readyState":"READY","created":800,"buildingAt":1000,"ready":63000,
			 "meta":{"githubCommitRef":"main","githubCommitSha":"8fa21c0aa","githubCommitMessage":"Add cart\nbody","githubCommitAuthorLogin":"jh"},
			 "creator":{"username":"someone"}},
			{"uid":"d2","url":"web-2.vercel.app","target":null,"readyState":"ERROR","created":700,"buildingAt":710,"ready":0,
			 "meta":{"gitCommitRef":"fix","gitCommitSha":"abcdef123","gitCommitMessage":"Fix","gitCommitAuthorName":"J H"},
			 "errorCode":"BUILD_UTILS_SPAWN_1","errorMessage":"Command \"bun run build\" exited with 1"}],
			"pagination":{"count":2,"next":690,"prev":800}}`), nil
	}
	got, err := readDeploymentsWith(t.Context(), DeploymentQuery{TeamID: "team_a", ProjectID: "prj_1", Environment: "production", State: "error", Cursor: "900", Limit: 2}, run)
	want := DeploymentPage{NextCursor: "690", Deployments: []DeploymentSummary{
		{ID: "d1", URL: "https://web-1.vercel.app", Environment: "production", State: "ready", At: time.UnixMilli(800), DurationSeconds: 62,
			Branch: "main", Commit: "8fa21c0", Message: "Add cart", Author: "jh"},
		{ID: "d2", URL: "https://web-2.vercel.app", Environment: "preview", State: "error", At: time.UnixMilli(700),
			Branch: "fix", Commit: "abcdef1", Message: "Fix", Author: "J H", Error: `Command "bun run build" exited with 1`},
	}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// The last page has no cursor, and an empty one is an empty list, not null.
func TestReadDeploymentsLastPage(t *testing.T) {
	got, err := readDeploymentsWith(t.Context(), DeploymentQuery{TeamID: "team_a", ProjectID: "prj_1", Limit: 500}, func(_ context.Context, args []string) ([]byte, error) {
		if args[1] != "/v6/deployments?limit=100&projectId=prj_1&teamId=team_a" {
			t.Fatalf("endpoint %s", args[1])
		}
		return []byte(`{"deployments":[],"pagination":{"count":0,"next":null,"prev":null}}`), nil
	})
	if err != nil || got.NextCursor != "" || got.Deployments == nil || len(got.Deployments) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestReadDeploymentsRefusesBadQueries(t *testing.T) {
	run := func(context.Context, []string) ([]byte, error) { t.Fatal("ran the CLI"); return nil, nil }
	for _, q := range []DeploymentQuery{
		{ProjectID: "prj_1"},
		{TeamID: "team_a", ProjectID: "prj_1&teamId=other"},
		{TeamID: "team_a", ProjectID: "prj_1", Environment: "staging"},
		{TeamID: "team_a", ProjectID: "prj_1", State: "READY"},
		{TeamID: "team_a", ProjectID: "prj_1", Cursor: "1&teamId=other"},
	} {
		if _, err := readDeploymentsWith(t.Context(), q, run); !errors.Is(err, errkind.ErrInvalid) {
			t.Fatalf("%+v: err = %v", q, err)
		}
	}
}
