package cloudprojects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/repogo/host/internal/cloudflare"
	"github.com/repogo/host/internal/cloudscan"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/fly"
	"github.com/repogo/host/internal/vercel"
)

func fixture(t *testing.T, root, path, content string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

type testPaths struct{ root string }

func (p testPaths) Contain(path string) (string, error) {
	root, err := filepath.EvalSymlinks(p.root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !files.Within(root, resolved) {
		return "", files.ErrOutsideScope
	}
	return resolved, nil
}

var now = time.Date(2026, 10, 1, 19, 0, 0, 0, time.UTC)

func service(root string) *Service {
	s := New(testPaths{root}, func(context.Context, string) bool { return true })
	s.vercel = func(_ context.Context, team, id string) (vercel.Project, error) {
		switch id {
		case "prj_web":
			return vercel.Project{Name: "web", URL: "https://example.com", Latest: &vercel.Deployment{State: "ERROR", At: now.Add(-3 * time.Minute), Reason: "Error: Failed to collect page data"}}, nil
		case "prj_gone":
			return vercel.Project{}, vercel.ErrProjectNotFound
		}
		return vercel.Project{}, errors.New("unexpected " + team + "/" + id)
	}
	s.fly = func(_ context.Context, app string) (fly.App, error) {
		if app == "api" {
			return fly.App{URL: "https://api.fly.dev", Latest: &fly.Release{Version: 3, Status: "complete", At: now.Add(-4 * time.Minute)}}, nil
		}
		return fly.App{}, errors.New("flyctl releases: not signed in\nrun fly auth login")
	}
	s.worker = func(_ context.Context, name, account string) (cloudflare.Worker, error) {
		return cloudflare.Worker{URL: "https://edge.team.workers.dev", Latest: &cloudflare.Deployment{VersionID: "42d90de6-ed06", At: now.Add(-2 * time.Hour)}}, nil
	}
	return s
}

func TestListReadsEachProvider(t *testing.T) {
	root := t.TempDir()
	fixture(t, root, "web/.vercel/project.json", `{"orgId":"team_a","projectId":"prj_web"}`)
	fixture(t, root, "web-copy/.vercel/project.json", `{"orgId":"team_a","projectId":"prj_web"}`)
	fixture(t, root, "gone/.vercel/project.json", `{"orgId":"team_a","projectId":"prj_gone","projectName":"gone"}`)
	fixture(t, root, "api/fly.toml", "app = \"api\"\n")
	fixture(t, root, "relay/fly.toml", "app = \"relay\"\n")
	fixture(t, root, "edge/wrangler.toml", "name = \"edge\"\n")
	fixture(t, root, "site/vercel.json", `{}`)

	got, err := service(root).List(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	cwd := got.Cwd
	want := []Project{
		{Provider: "fly", ID: "api", Name: "api", URL: "https://api.fly.dev", Directory: filepath.Join(cwd, "api"),
			Deployment: &Deployment{State: "deployed", Detail: "v3", At: now.Add(-4 * time.Minute)}},
		{Provider: "cloudflare", ID: "edge", Name: "edge", URL: "https://edge.team.workers.dev", Directory: filepath.Join(cwd, "edge"),
			Deployment: &Deployment{State: "deployed", Detail: "42d90de6", At: now.Add(-2 * time.Hour)}},
		{Provider: "vercel", TeamID: "team_a", ID: "prj_gone", Name: "gone", Directory: filepath.Join(cwd, "gone"), Error: "Vercel has no project with this id; link the folder again"},
		{Provider: "fly", ID: "relay", Name: "relay", Directory: filepath.Join(cwd, "relay"), Error: "flyctl releases: not signed in"},
		{Provider: "vercel", Name: "site", Directory: filepath.Join(cwd, "site")},
		{Provider: "vercel", TeamID: "team_a", ID: "prj_web", Name: "web", URL: "https://example.com", Directory: filepath.Join(cwd, "web"),
			Deployment: &Deployment{State: "build_failed", Detail: "Error: Failed to collect page data", At: now.Add(-3 * time.Minute)}},
	}
	if !reflect.DeepEqual(got.Projects, want) {
		t.Fatalf("projects =\n%+v\nwant\n%+v", got.Projects, want)
	}
}

// A provider whose CLI is missing or signed out has no cards and isn't read;
// it is named so the sheet can offer its sign-in.
func TestListShowsOnlyProvidersWhoseCLIIsReady(t *testing.T) {
	root := t.TempDir()
	fixture(t, root, "web/.vercel/project.json", `{"orgId":"team_a","projectId":"prj_web"}`)
	fixture(t, root, "api/fly.toml", "app = \"api\"\n")
	fixture(t, root, "edge/wrangler.toml", "name = \"edge\"\n")

	s := service(root)
	s.ready = func(_ context.Context, provider string) bool { return provider == "fly" }
	s.vercel = func(context.Context, string, string) (vercel.Project, error) {
		t.Error("read Vercel although its CLI isn't ready")
		return vercel.Project{}, nil
	}
	got, err := s.List(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Projects) != 1 || got.Projects[0].Provider != "fly" {
		t.Fatalf("projects = %+v, want only fly's", got.Projects)
	}
	if !reflect.DeepEqual(got.NeedsSetup, []string{"cloudflare", "vercel"}) {
		t.Fatalf("needs setup = %v", got.NeedsSetup)
	}
}

func TestDeploymentsRemainVisible(t *testing.T) {
	old := now.Add(-24 * time.Hour)
	for _, tc := range []struct {
		name string
		got  *Deployment
		want string
	}{
		{"vercel never deployed", vercelDeployment(nil), ""},
		{"vercel failed", vercelDeployment(&vercel.Deployment{State: "ERROR", At: old}), "build_failed"},
		{"vercel old success", vercelDeployment(&vercel.Deployment{State: "READY", At: old}), "deployed"},
		{"vercel building", vercelDeployment(&vercel.Deployment{State: "BUILDING", At: now}), "building"},
		{"vercel queued", vercelDeployment(&vercel.Deployment{State: "QUEUED", At: now}), "queued"},
		{"vercel initializing", vercelDeployment(&vercel.Deployment{State: "INITIALIZING", At: now}), "initializing"},
		{"vercel canceled", vercelDeployment(&vercel.Deployment{State: "CANCELED", At: now}), "canceled"},
		{"fly never deployed", flyDeployment(nil), ""},
		{"fly failed", flyDeployment(&fly.Release{Version: 2, Status: "failed", At: old}), "deploy_failed"},
		{"fly old success", flyDeployment(&fly.Release{Version: 2, Status: "complete", At: old}), "deployed"},
		{"fly running release", flyDeployment(&fly.Release{Version: 2, Status: "running", At: now}), "deploying"},
		{"worker never deployed", workerDeployment(nil), ""},
		{"worker old success", workerDeployment(&cloudflare.Deployment{VersionID: "v", At: old}), "deployed"},
	} {
		state := ""
		if tc.got != nil {
			state = tc.got.State
		}
		if state != tc.want {
			t.Errorf("%s: state %q, want %q", tc.name, state, tc.want)
		}
	}
}

func TestFlyMachinesReachCard(t *testing.T) {
	s := service(t.TempDir())
	machines := &fly.MachineSummary{Count: 2, Running: 1, State: "mixed", Regions: []string{"iad"}, CPUs: 4, MemoryMB: 2048}
	s.fly = func(context.Context, string) (fly.App, error) {
		return fly.App{Machines: machines}, nil
	}
	got := s.read(t.Context(), cloudscan.Project{Provider: "fly", ID: "api"})
	if !reflect.DeepEqual(got.Machines, machines) || got.Deployment != nil {
		t.Fatalf("card = %+v", got)
	}
}

func TestListOutsideScope(t *testing.T) {
	if _, err := service(t.TempDir()).List(t.Context(), t.TempDir()); !errors.Is(err, files.ErrOutsideScope) {
		t.Fatalf("err = %v", err)
	}
	if _, err := service(t.TempDir()).List(t.Context(), ""); !errors.Is(err, cloudscan.ErrInvalidPath) {
		t.Fatalf("err = %v", err)
	}
}
