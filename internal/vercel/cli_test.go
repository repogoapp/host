package vercel

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReadProject(t *testing.T) {
	run := func(_ context.Context, args []string) ([]byte, error) {
		expected := []string{"api", "/v9/projects/p1?teamId=team_a", "--raw", "--non-interactive"}
		if !reflect.DeepEqual(args, expected) {
			t.Fatalf("args: %v", args)
		}
		return []byte(`{"id":"p1","name":"web","framework":"nextjs",
			"targets":{"production":{"alias":["web-team.vercel.app","example.com","www.example.com"]}},
			"latestDeployments":[
				{"id":"d_preview","readyState":"ERROR","target":null,"createdAt":3000},
				{"id":"d_new","readyState":"READY","target":"production","createdAt":2000,"readyAt":2500,
				 "meta":{"githubCommitSha":"8fa21c0aa","githubCommitMessage":"Add cart\n\nLonger body"}},
				{"id":"d_old","readyState":"ERROR","target":"production","createdAt":1000}]}`), nil
	}
	got, err := readWith(t.Context(), "team_a", "p1", run)
	want := Project{Name: "web", Framework: "nextjs", URL: "https://example.com",
		Latest: &Deployment{ID: "d_new", State: "READY", At: time.UnixMilli(2500), Commit: "8fa21c0", Message: "Add cart"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v %+v, %v", got, got.Latest, err)
	}
}

// A project never deployed has no production target and no deployments.
func TestReadProjectNeverDeployed(t *testing.T) {
	got, err := readWith(t.Context(), "team_a", "p1", func(context.Context, []string) ([]byte, error) {
		return []byte(`{"id":"p1","name":"web"}`), nil
	})
	if err != nil || !reflect.DeepEqual(got, Project{Name: "web"}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// A failed production build reads its log for the line that says why,
// skipping the generic exit line Vercel prints last.
func TestReadProjectFailedBuildReason(t *testing.T) {
	run := func(_ context.Context, args []string) ([]byte, error) {
		if strings.HasPrefix(args[1], "/v3/deployments/d_bad/events") {
			return []byte(`[
				{"type":"stderr","text":"Error: Command \"bun run build\" exited with 1"},
				{"type":"stderr","text":"    at ignore-listed frames"},
				{"type":"stderr","text":"Error: Failed to collect page data for /api/login"},
				{"type":"stdout","text":"Error-free line on stdout"}]`), nil
		}
		return []byte(`{"id":"p1","name":"web","latestDeployments":[{"id":"d_bad","readyState":"ERROR","target":"production","createdAt":1000}]}`), nil
	}
	got, err := readWith(t.Context(), "team_a", "p1", run)
	if err != nil || got.Latest == nil || got.Latest.Reason != "Error: Failed to collect page data for /api/login" {
		t.Fatalf("got %+v, %v", got.Latest, err)
	}
}

// A deploy from the CLI names its commit gitCommitSha, not the provider's key.
func TestReadProjectCLIDeployCommit(t *testing.T) {
	got, err := readWith(t.Context(), "team_a", "p1", func(context.Context, []string) ([]byte, error) {
		return []byte(`{"id":"p1","name":"web","latestDeployments":[{"id":"d1","readyState":"BUILDING","target":"production","createdAt":1000,
			"meta":{"gitCommitSha":"abcdef123","gitCommitMessage":"Fix checkout"}}]}`), nil
	})
	if err != nil || got.Latest == nil || got.Latest.Commit != "abcdef1" || got.Latest.Message != "Fix checkout" || got.Latest.State != "BUILDING" {
		t.Fatalf("got %+v, %v", got.Latest, err)
	}
}

func TestProductionURL(t *testing.T) {
	for _, tc := range []struct {
		aliases []string
		want    string
	}{
		{nil, ""},
		{[]string{"web.vercel.app"}, "https://web.vercel.app"},
		{[]string{"web.vercel.app", "example.com"}, "https://example.com"},
	} {
		if got := productionURL(tc.aliases); got != tc.want {
			t.Errorf("productionURL(%v) = %q, want %q", tc.aliases, got, tc.want)
		}
	}
}
