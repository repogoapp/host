package connect

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/envsource"
	"github.com/repogoapp/host/internal/errkind"
)

var ctx = context.Background()

type fixture struct {
	s       *Service
	mcp     *fakeMCP
	clis    *fakeCLIs
	home    string
	project string
	plugin  string // the stripe plugin's folder
	shared  string
	now     time.Time
	events  []Flow
	// ignored is what git ignores, by path relative to its repository.
	ignored map[string]bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	f := &fixture{
		home:    home,
		ignored: map[string]bool{},
		project: filepath.Join(home, "app"),
		plugin:  filepath.Join(home, "plugins", "stripe"),
		shared:  filepath.Join(home, "plugins", ".env"),
		now:     time.Unix(1_800_000_000, 0),
	}
	for _, dir := range []string{f.project, f.plugin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := envsource.Open(filepath.Join(home, "env-sources.json"), home)
	if err != nil {
		t.Fatal(err)
	}
	f.mcp = &fakeMCP{}
	f.clis = &fakeCLIs{installs: map[string]clitool.Install{}}
	f.s = New(Deps{
		Sources: sources,
		MCP:     f.mcp,
		CLIs:    f.clis,
		PluginDir: func(project, plugin string) (string, error) {
			if plugin != "stripe" {
				return "", errkind.ErrNotFound.Errorf("no plugin %q here", plugin)
			}
			return f.plugin, nil
		},
		SharedEnv: f.shared,
		Contain: func(path string) (string, error) {
			if path != f.project {
				return "", errkind.ErrDenied.Errorf("not a project")
			}
			return path, nil
		},
		Home: home,
		Ignored: func(_ context.Context, _ string, paths []string) (map[string]bool, error) {
			return f.ignored, nil
		},
		Changed: func(c Changed) { f.events = append(f.events, c.Flow) },
		Now:     func() time.Time { return f.now },
	})
	return f
}

func (f *fixture) start(t *testing.T, target, scope string) Flow {
	t.Helper()
	flow, err := f.s.Start(context.Background(), StartParams{Project: f.project, Target: target, Scope: scope,
		Note: "Make a restricted key", HelpURL: "https://dashboard.stripe.com/apikeys"})
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSubmitWritesTheKeyAndNeverReportsIt(t *testing.T) {
	f := newFixture(t)
	flow := f.start(t, "env:STRIPE_SECRET_KEY", "plugin:stripe")
	if flow.State != Waiting || flow.Step == nil || flow.Step.Kind != StepSecret {
		t.Fatalf("start = %+v", flow)
	}
	if flow.Destination != "The stripe plugin's .env" {
		t.Fatalf("destination = %q", flow.Destination)
	}

	done, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "rk_live_secret")
	if err != nil {
		t.Fatal(err)
	}
	if done.State != Succeeded || done.Step != nil {
		t.Fatalf("submit = %+v", done)
	}
	path := filepath.Join(f.plugin, ".env")
	if got := read(t, path); got != "STRIPE_SECRET_KEY=rk_live_secret\n" {
		t.Fatalf("file = %q", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
	got := f.s.List(ctx, f.project, []Want{{Target: "env:STRIPE_SECRET_KEY", Scope: "plugin:stripe"}})
	if len(got) != 1 || got[0].Status != Configured {
		t.Fatalf("list = %+v", got)
	}

	// Whatever leaves the service, the value is never in it.
	out, _ := json.Marshal([]any{done, got, f.events})
	if strings.Contains(string(out), "rk_live_secret") {
		t.Fatalf("the value left the service: %s", out)
	}
	if len(f.events) != 2 || f.events[0].State != Waiting || f.events[1].State != Succeeded {
		t.Fatalf("events = %+v", f.events)
	}
}

func TestSubmitFillsAnEmptyKeyAndKeepsTheRest(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.project, ".env.local")
	before := "# keys\nexport OPENAI_KEY=\nDATABASE_URL=postgres://x\nOPENAI_KEY=\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	flow := f.start(t, "env:OPENAI_KEY", "file:.env.local")
	if _, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "sk new#1"); err != nil {
		t.Fatal(err)
	}
	want := "# keys\nOPENAI_KEY='sk new#1'\nDATABASE_URL=postgres://x\n# OPENAI_KEY=\n"
	if got := read(t, path); got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
	if v := envsource.Parse(read(t, path))["OPENAI_KEY"]; v != "sk new#1" {
		t.Fatalf("parsed = %q", v)
	}
}

func TestSharedAndSourceScopes(t *testing.T) {
	f := newFixture(t)
	flow := f.start(t, "env:ANTHROPIC_API_KEY", "plugins")
	if _, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "sk-ant"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f.shared); got != "ANTHROPIC_API_KEY=sk-ant\n" {
		t.Fatalf("shared = %q", got)
	}

	bound := filepath.Join(filepath.Dir(f.project), "secrets.env")
	if err := os.WriteFile(bound, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.d.Sources.Set("my-secrets", bound); err != nil {
		t.Fatal(err)
	}
	flow = f.start(t, "env:B", "source:my-secrets")
	if _, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "2"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, bound); got != "A=1\nB=2\n" {
		t.Fatalf("source = %q", got)
	}
}

func TestStartRefusesWhatItCantResolve(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct{ target, scope string }{
		{"ssh:host", "plugins"},
		{"mcp:", ""},
		{"cli:../gh", ""},
		{"env:lower_case", "plugins"},
		{"env:KEY", ""},
		{"env:KEY", "plugin:../x"},
		{"env:KEY", "plugin:missing"},
		{"env:KEY", "file:../.env"},
		{"env:KEY", "file:/etc/hosts"},
		{"env:KEY", "file:."},
		{"env:KEY", "project:.env"},
		{"env:KEY", "source:missing"},
	} {
		if _, err := f.s.Start(ctx, StartParams{Project: f.project, Target: c.target, Scope: c.scope}); err == nil {
			t.Errorf("%s in %q: started", c.target, c.scope)
		}
	}
	if _, err := f.s.Start(ctx, StartParams{Project: "/elsewhere", Target: "env:KEY", Scope: "file:.env"}); err == nil {
		t.Error("a folder that is not a project: started")
	}
	if _, err := f.s.Start(ctx, StartParams{Project: f.project, Target: "env:KEY", Scope: "plugins", HelpURL: "http://x.test"}); err == nil {
		t.Error("an http help link: started")
	}
	if len(f.events) != 0 {
		t.Fatalf("events = %+v", f.events)
	}
}

func TestListSaysWhyATargetIsUnavailable(t *testing.T) {
	f := newFixture(t)
	got := f.s.List(ctx, f.project, []Want{
		{Target: "env:STRIPE_SECRET_KEY", Scope: "plugin:stripe"},
		{Target: "env:KEY", Scope: "plugin:missing"},
	})
	if got[0].Status != Missing || got[0].Destination == "" {
		t.Fatalf("missing = %+v", got[0])
	}
	if got[1].Status != Unavailable || got[1].Reason == "" {
		t.Fatalf("unavailable = %+v", got[1])
	}
}

func TestValuesALineCantHold(t *testing.T) {
	f := newFixture(t)
	flow := f.start(t, "env:KEY", "plugins")
	for _, bad := range []string{"", "a\nb", "a\rb", `it's "both"`, strings.Repeat("x", maxValue+1)} {
		if _, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
	// A refused value leaves the flow waiting for another.
	if got, _ := f.s.Get(flow.ID); got.State != Waiting {
		t.Fatalf("state = %s", got.State)
	}
	if _, err := os.Stat(f.shared); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused value wrote the file")
	}
}

func TestALinkIsNeverWritten(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(filepath.Dir(f.project), "elsewhere.env")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(f.project, ".env")); err != nil {
		t.Fatal(err)
	}
	flow := f.start(t, "env:KEY", "file:.env")
	done, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "v")
	if err != nil {
		t.Fatal(err)
	}
	if done.State != Failed || done.Reason == "" {
		t.Fatalf("submit = %+v", done)
	}
	if read(t, target) != "" {
		t.Fatal("the link's target was written")
	}
}

func TestCancelExpiryAndForgetting(t *testing.T) {
	f := newFixture(t)
	flow := f.start(t, "env:KEY", "plugins")
	if err := f.s.Cancel(flow.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "v"); err == nil {
		t.Fatal("a cancelled flow took a value")
	}
	if got, _ := f.s.Get(flow.ID); got.State != Cancelled {
		t.Fatalf("state = %s", got.State)
	}

	waiting := f.start(t, "env:OTHER", "plugins")
	f.now = f.now.Add(waitFor)
	if got, _ := f.s.Get(waiting.ID); got.State != Expired {
		t.Fatalf("state = %s", got.State)
	}
	if last := f.events[len(f.events)-1]; last.ID != waiting.ID || last.State != Expired {
		t.Fatalf("last event = %+v", last)
	}

	f.now = f.now.Add(kept)
	if _, err := f.s.Get(waiting.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get = %v", err)
	}
	if err := f.s.Cancel("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel = %v", err)
	}
}

func TestSubmitNamesItsStep(t *testing.T) {
	f := newFixture(t)
	flow := f.start(t, "env:KEY", "plugins")
	if _, err := f.s.Submit(ctx, flow.ID, "other", "v"); err == nil {
		t.Fatal("a step that isn't waiting took a value")
	}
	if _, err := f.s.Submit(ctx, "missing", flow.Step.ID, "v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("submit = %v", err)
	}
}

func TestFilePathsInTheProjectAndUnderHome(t *testing.T) {
	f := newFixture(t)
	flow := f.start(t, "env:DATABASE_URL", "file:apps/web/.env.local")
	if !strings.HasSuffix(flow.Destination, "app/apps/web/.env.local") || !strings.HasPrefix(flow.Destination, "~/") {
		t.Fatalf("destination = %q", flow.Destination)
	}
	if _, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "postgres://x"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(f.project, "apps", "web", ".env.local")); got != "DATABASE_URL=postgres://x\n" {
		t.Fatalf("file = %q", got)
	}

	flow = f.start(t, "env:STRIPE_KEY", "file:~/work/api/.dev.vars")
	if flow.Destination != "~/work/api/.dev.vars" {
		t.Fatalf("destination = %q", flow.Destination)
	}
	if _, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "sk"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(f.home, "work", "api", ".dev.vars")); got != "STRIPE_KEY=sk\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestALinkedFolderOutOfHomeIsRefused(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(f.project, "out")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Start(ctx, StartParams{Project: f.project, Target: "env:KEY", Scope: "file:out/.env"}); err == nil {
		t.Fatal("wrote through a link out of home")
	}
}

func TestWarnsWhenGitWouldCommitTheFile(t *testing.T) {
	f := newFixture(t)
	if err := os.Mkdir(filepath.Join(f.project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if flow := f.start(t, "env:KEY", "file:.env"); flow.Step.Warning == "" {
		t.Fatal("no warning for a file git would commit")
	}
	f.ignored[".env"] = true
	if flow := f.start(t, "env:KEY", "file:.env"); flow.Step.Warning != "" {
		t.Fatalf("warning for an ignored file: %q", flow.Step.Warning)
	}
	if flow := f.start(t, "env:KEY", "plugins"); flow.Step.Warning != "" {
		t.Fatalf("warning outside a repository: %q", flow.Step.Warning)
	}
}

func TestAProjectLinkToAnotherFolderIsOutsideTheProject(t *testing.T) {
	f := newFixture(t)
	other := filepath.Join(f.home, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(f.project, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Start(ctx, StartParams{Project: f.project, Target: "env:KEY", Scope: "file:linked/.env"}); err == nil {
		t.Fatal("wrote through a project link to another folder")
	}
}

func TestAPathThatChangesAfterStartIsRefused(t *testing.T) {
	f := newFixture(t)
	web := filepath.Join(f.project, "web")
	if err := os.Mkdir(web, 0o700); err != nil {
		t.Fatal(err)
	}
	flow := f.start(t, "env:KEY", "file:web/.env")
	// The folder is swapped for a link to somewhere else under home.
	elsewhere := filepath.Join(f.home, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(web); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, web); err != nil {
		t.Fatal(err)
	}
	done, err := f.s.Submit(ctx, flow.ID, flow.Step.ID, "v")
	if err != nil || done.State != Failed {
		t.Fatalf("submit = %+v, %v", done, err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the value went to where the link points")
	}
}

func TestASetKeyOpensConnectedAndCanBeRemoved(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.project, ".env")
	if err := os.WriteFile(path, []byte("A=1\nKEY=secret\nB=2\nexport KEY=again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	flow := f.start(t, "env:KEY", "file:.env")
	if flow.State != Succeeded || flow.Step != nil {
		t.Fatalf("start = %+v", flow)
	}
	done, err := f.s.Disconnect(ctx, flow.ID)
	if err != nil || done.State != Disconnected {
		t.Fatalf("disconnect = %+v, %v", done, err)
	}
	if got := read(t, path); got != "A=1\nB=2\n" {
		t.Fatalf("file = %q", got)
	}
	if again, err := f.s.Disconnect(ctx, flow.ID); err != nil || again.State != Disconnected {
		t.Fatalf("again = %+v, %v", again, err)
	}
}

func TestOnlyAConnectedFlowDisconnects(t *testing.T) {
	f := newFixture(t)
	flow := f.start(t, "env:KEY", "plugins")
	if _, err := f.s.Disconnect(ctx, flow.ID); err == nil {
		t.Fatal("a waiting flow disconnected")
	}
	if _, err := f.s.Disconnect(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestTickExpiresButNeverMidSave(t *testing.T) {
	f := newFixture(t)
	idle := f.start(t, "env:A", "plugins")
	saving := f.start(t, "env:B", "plugins")
	f.s.mu.Lock()
	f.s.flows[saving.ID].working = true
	f.s.mu.Unlock()
	f.now = f.now.Add(waitFor)
	f.s.Tick(ctx, f.now)
	f.s.mu.Lock()
	idleState, savingState := f.s.flows[idle.ID].State, f.s.flows[saving.ID].State
	f.s.mu.Unlock()
	if idleState != Expired || savingState != Waiting {
		t.Fatalf("idle %s, saving %s", idleState, savingState)
	}
}

func TestRemovingAKeyWhoseFolderMovedIsRefused(t *testing.T) {
	f := newFixture(t)
	web := filepath.Join(f.project, "web")
	if err := os.Mkdir(web, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, ".env"), []byte("KEY=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	flow := f.start(t, "env:KEY", "file:web/.env")
	elsewhere := filepath.Join(f.home, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, ".env"), []byte("KEY=keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(web, filepath.Join(f.project, "web-old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, web); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Disconnect(ctx, flow.ID); err == nil {
		t.Fatal("removed a key through a swapped folder")
	}
	if got := read(t, filepath.Join(elsewhere, ".env")); got != "KEY=keep\n" {
		t.Fatalf("the link's file changed: %q", got)
	}
}

func TestTheSweepLeavesAWorkingFlowAlone(t *testing.T) {
	f := newFixture(t)
	flow := f.start(t, "env:OTHER", "plugins")
	f.s.mu.Lock()
	f.s.flows[flow.ID].working = true
	f.s.mu.Unlock()
	f.now = f.now.Add(waitFor + kept)
	f.s.Tick(ctx, f.now)
	if _, err := f.s.Get(flow.ID); err != nil {
		t.Fatalf("a working flow was swept: %v", err)
	}
}
