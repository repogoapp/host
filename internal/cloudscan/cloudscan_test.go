package cloudscan

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/repogo/host/internal/files"
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

func TestDetectReadsEachProvider(t *testing.T) {
	root := t.TempDir()
	fixture(t, root, "web/.vercel/project.json", `{"orgId":"team_a","projectId":"prj_1","projectName":"web"}`)
	fixture(t, root, "web/apps/site/vercel.json", `{}`)
	fixture(t, root, "loose/vercel.json", `{}`)
	fixture(t, root, "api/fly.toml", "# fly.toml app configuration\napp = \"api-app\" # set by fly launch\nprimary_region = 'iad'\n\n[env]\napp = \"not-this\"\n")
	fixture(t, root, "unlaunched/fly.toml", "primary_region = \"iad\"\n")
	fixture(t, root, "worker/wrangler.toml", "name = \"edge\"\naccount_id = \"acc_1\"\n[vars]\nname = \"not-this\"\n")
	fixture(t, root, "jsonc/wrangler.jsonc", "{\n  // the Worker\n  \"name\": \"hono\", /* inline */\n  \"main\": \"src/index.ts\",\n  \"routes\": [\"a.example.com/*\",],\n}\n")
	fixture(t, root, "json/wrangler.json", `{"name":"plain"}`)

	got, err := New(testPaths{root}).Detect(t.Context(), root)
	if err != nil || len(got.Issues) != 0 {
		t.Fatalf("detect: %+v %v", got, err)
	}
	cwd := got.Cwd
	want := []Project{
		{Provider: ProviderFly, Name: "api-app", ID: "api-app", Directory: filepath.Join(cwd, "api"), ConfigPath: filepath.Join(cwd, "api/fly.toml")},
		{Provider: ProviderCloudflare, Name: "plain", ID: "plain", Directory: filepath.Join(cwd, "json"), ConfigPath: filepath.Join(cwd, "json/wrangler.json")},
		{Provider: ProviderCloudflare, Name: "hono", ID: "hono", Directory: filepath.Join(cwd, "jsonc"), ConfigPath: filepath.Join(cwd, "jsonc/wrangler.jsonc")},
		{Provider: ProviderVercel, Directory: filepath.Join(cwd, "loose"), ConfigPath: filepath.Join(cwd, "loose/vercel.json")},
		{Provider: ProviderFly, Directory: filepath.Join(cwd, "unlaunched"), ConfigPath: filepath.Join(cwd, "unlaunched/fly.toml")},
		{Provider: ProviderVercel, Name: "web", ID: "prj_1", TeamID: "team_a", Directory: filepath.Join(cwd, "web"), ConfigPath: filepath.Join(cwd, "web/.vercel/project.json")},
		{Provider: ProviderCloudflare, Name: "edge", ID: "edge", TeamID: "acc_1", Directory: filepath.Join(cwd, "worker"), ConfigPath: filepath.Join(cwd, "worker/wrangler.toml")},
	}
	if !slices.Equal(got.Projects, want) {
		t.Fatalf("projects =\n%+v\nwant\n%+v", got.Projects, want)
	}
}

func TestScanKeepsLinkedVercelConfig(t *testing.T) {
	root := t.TempDir()
	fixture(t, root, "web/.vercel/project.json", `{"orgId":"team_a","projectId":"prj_1"}`)
	fixture(t, root, "web/apps/site/vercel.json", `{}`)
	got, err := New(testPaths{root}).scan(t.Context(), root)
	if err != nil || len(got.Projects) != 2 {
		t.Fatalf("scan keeps every config for the Vercel list: %+v %v", got, err)
	}
}

func TestBrokenConfigIsAnIssue(t *testing.T) {
	root := t.TempDir()
	fixture(t, root, "a/wrangler.json", `{`)
	fixture(t, root, "b/fly.toml", "app = \"open\n")
	fixture(t, root, "c/.vercel/project.json", `{"projectId":"p"}`)
	got, err := New(testPaths{root}).Detect(t.Context(), root)
	if err != nil || len(got.Projects) != 0 || len(got.Issues) != 3 {
		t.Fatalf("detect: %+v %v", got, err)
	}
}

// A clone kept in an ignored folder must not show up as the project's config,
// while the ignored .vercel that `vercel link` writes still must.
func TestScanFollowsGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	gitInit := func(dir string) {
		if out, err := exec.Command("git", "init", "-q", filepath.Join(root, dir)).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
	}
	gitInit("repo")
	fixture(t, root, "repo/.gitignore", ".repos/\n.vercel\n")
	fixture(t, root, "repo/fly.toml", "app = \"repo-app\"\n")
	fixture(t, root, "repo/.repos/clone/fly.toml", "app = \"clone\"\n")
	fixture(t, root, "repo/.repos/clone/.vercel/project.json", `{"orgId":"team_x","projectId":"px"}`)
	fixture(t, root, "repo/apps/web/.vercel/project.json", `{"orgId":"team_a","projectId":"p1"}`)
	gitInit("repo/nested")
	fixture(t, root, "repo/nested/wrangler.toml", "name = \"nested\"\n")
	fixture(t, root, "loose/vercel.json", `{}`)

	got, err := New(testPaths{root}).Detect(t.Context(), root)
	if err != nil || len(got.Issues) != 0 {
		t.Fatalf("detect: %+v %v", got, err)
	}
	ids := []string{}
	for _, p := range got.Projects {
		ids = append(ids, p.Provider+":"+p.ID)
	}
	if want := []string{"vercel:", "fly:repo-app", "vercel:p1", "cloudflare:nested"}; !slices.Equal(ids, want) {
		t.Fatalf("projects = %v, want %v", ids, want)
	}
}

func TestVercelRepoMappingStaysInside(t *testing.T) {
	root := t.TempDir()
	fixture(t, root, "repo/apps/web/vercel.json", `{}`)
	fixture(t, root, "bad/x", ``)
	fixture(t, root, "repo/.vercel/repo.json", `{"orgId":"team_a","projects":[{"id":"p1","name":"web","directory":"apps/web"}]}`)
	scoped, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer scoped.Close()
	projects, err := readVercelLink(scoped, root, "repo/.vercel/repo.json")
	if err != nil || len(projects) != 1 || projects[0].Directory != filepath.Join(root, "repo/apps/web") {
		t.Fatalf("mapping: %+v %v", projects, err)
	}
	fixture(t, root, "repo/.vercel/repo.json", `{"orgId":"team_a","projects":[{"id":"p1","directory":"../bad"}]}`)
	if _, err := readVercelLink(scoped, root, "repo/.vercel/repo.json"); err == nil {
		t.Fatal("accepted escaping repo mapping")
	}
}

func TestStripJSONCLeavesStrings(t *testing.T) {
	in := `{"url": "https://a.dev/*", "note": "a /* b */ c", // gone
"list": [1, 2,], /* gone */ "end": "x\"//y",}`
	want := `{"url": "https://a.dev/*", "note": "a /* b */ c",
"list": [1, 2],  "end": "x\"//y"}`
	if got := string(stripJSONC([]byte(in))); got != want {
		t.Fatalf("stripJSONC =\n%s\nwant\n%s", got, want)
	}
}
