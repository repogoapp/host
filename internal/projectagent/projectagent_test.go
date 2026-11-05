package projectagent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// write puts an agent.json for name in plugins, the environment's folder.
func write(t *testing.T, plugins, name, manifest string) {
	t.Helper()
	folder := filepath.Join(plugins, name)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, ManifestFile), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestListShowsEachAgentAndWhyABrokenOneCantRun(t *testing.T) {
	plugins, dir := t.TempDir(), t.TempDir()
	write(t, plugins, "helper", `{"name":"Helper","command":"npx","args":["eve","acp"],"cwd":"agent"}`)
	write(t, plugins, "plain", `{"command":"gemini"}`)
	write(t, plugins, "broken", `{"name":"Broken"}`)
	write(t, plugins, "Bad Name", `{"command":"x"}`)
	if err := os.MkdirAll(filepath.Join(plugins, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := List(dir, plugins)
	if err != nil {
		t.Fatal(err)
	}
	want := []ProjectAgent{
		{ID: "broken", Name: "broken", Error: "agent.json: command is empty"},
		{ID: "helper", Name: "Helper"},
		{ID: "plain", Name: "plain"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A repo can't bring an agent: an agent.json in its .repogo/agents or
// .repogo/plugins would name a program its author chose, so it never runs.
func TestARepositorysOwnAgentIsNeverRead(t *testing.T) {
	plugins, dir := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, ".repogo", "agents"), "evil", `{"command":"./run.sh"}`)
	write(t, filepath.Join(dir, ".repogo", "plugins"), "sneaky", `{"command":"./run.sh"}`)
	write(t, filepath.Join(dir, ".repogo", "agents"), "muse", `{"name":"Repo muse","command":"./run.sh"}`)
	write(t, plugins, "muse", `{"name":"Muse","command":"muse-acp"}`)
	got, err := List(dir, plugins)
	if err != nil || !reflect.DeepEqual(got, []ProjectAgent{{ID: "muse", Name: "Muse"}}) {
		t.Fatalf("listed %+v %v", got, err)
	}
	for _, name := range []string{"evil", "sneaky"} {
		if _, err := Find(dir, plugins, name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s found: %v", name, err)
		}
	}
	if muse, err := Find(dir, plugins, "muse"); err != nil || muse.Dir != filepath.Join(plugins, "muse") {
		t.Fatalf("muse %+v %v", muse, err)
	}
}

// An agent works on the project's checkout, found from any subfolder, or in
// its own folder when it says so.
func TestFindResolvesWhereTheAgentRuns(t *testing.T) {
	plugins, dir := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "src")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, plugins, "helper", `{"command":"npx","cwd":"agent"}`)
	write(t, plugins, "coder", `{"command":"gemini"}`)
	helper, err := Find(sub, plugins, "helper")
	if err != nil {
		t.Fatal(err)
	}
	if helper.Workdir() != filepath.Join(plugins, "helper") || helper.Name != "helper" || helper.Fingerprint == "" {
		t.Fatalf("helper %+v", helper)
	}
	coder, err := Find(sub, plugins, "coder")
	if err != nil || coder.Workdir() != dir || coder.Cwd != CwdProject {
		t.Fatalf("coder %+v %v", coder, err)
	}
	if coder.Fingerprint == helper.Fingerprint {
		t.Fatal("two manifests share a fingerprint")
	}
	for _, name := range []string{"missing", "../escape", ""} {
		if _, err := Find(dir, plugins, name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q: %v", name, err)
		}
	}
	write(t, plugins, "odd", `{"command":"x","cwd":"home"}`)
	if _, err := Find(dir, plugins, "odd"); err == nil {
		t.Fatal("unknown cwd accepted")
	}
}

func TestListWithoutAgentsIsEmpty(t *testing.T) {
	for _, plugins := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		if got, err := List(t.TempDir(), plugins); err != nil || got != nil {
			t.Fatalf("got %+v %v", got, err)
		}
	}
}

func TestModelsListTheirDefaultAndNeedTheirVariable(t *testing.T) {
	plugins, dir := t.TempDir(), t.TempDir()
	write(t, plugins, "helper", `{"command":"npx","modelEnv":"HELPER_MODEL","models":[{"id":"claude-sonnet-5-5","name":"Sonnet"},{"id":"claude-haiku-5-5"}]}`)
	write(t, plugins, "novar", `{"command":"npx","models":[{"id":"x"}]}`)
	write(t, plugins, "twice", `{"command":"npx","modelEnv":"M","models":[{"id":"x"},{"id":"x"}]}`)
	got, err := List(dir, plugins)
	if err != nil {
		t.Fatal(err)
	}
	helper := got[0]
	want := []ProjectAgentModel{{ID: "claude-sonnet-5-5", Name: "Sonnet"}, {ID: "claude-haiku-5-5", Name: "claude-haiku-5-5"}}
	if helper.ID != "helper" || helper.Model != "claude-sonnet-5-5" || !reflect.DeepEqual(helper.Models, want) {
		t.Fatalf("helper %+v", helper)
	}
	if got[1].ID != "novar" || got[1].Error == "" || got[2].ID != "twice" || got[2].Error == "" {
		t.Fatalf("bad manifests %+v", got[1:])
	}
	m, err := Find(dir, plugins, "helper")
	if err != nil {
		t.Fatal(err)
	}
	for asked, runs := range map[string]string{"": "claude-sonnet-5-5", "claude-haiku-5-5": "claude-haiku-5-5", "gone": "claude-sonnet-5-5"} {
		if got := m.DefaultModel(asked); got != runs {
			t.Fatalf("asked %q runs %q, want %q", asked, got, runs)
		}
	}
}

// An agent working on the project's code runs once per project; one that
// runs in its own folder is one process for every project.
func TestAgentKeyFollowsWhereItRuns(t *testing.T) {
	plugins := t.TempDir()
	write(t, plugins, "coder", `{"command":"gemini"}`)
	write(t, plugins, "app", `{"command":"npx","cwd":"agent"}`)
	first, second := t.TempDir(), t.TempDir()
	key := func(project, name string) Manifest {
		m, err := Find(project, plugins, name)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if a, b := key(first, "coder"), key(second, "coder"); a.Key() == b.Key() || a.Fingerprint == b.Fingerprint {
		t.Fatalf("coder shares a process across projects: %q", a.Key())
	}
	if a, b := key(first, "app"), key(second, "app"); a.Key() != b.Key() || a.Fingerprint != b.Fingerprint {
		t.Fatalf("app runs twice in its folder: %q %q", a.Key(), b.Key())
	}
}

// An agent and its models may name an icon: an https image or a logo the app
// ships. Anything else lists the agent with why it can't run.
func TestIconsAreAnHTTPSImageOrALogoName(t *testing.T) {
	plugins, dir := t.TempDir(), t.TempDir()
	write(t, plugins, "muse", `{"command":"muse-acp","icon":"https://example.com/muse.png","modelEnv":"M","models":[{"id":"sonnet","icon":"claude"},{"id":"plain"}]}`)
	write(t, plugins, "http", `{"command":"x","icon":"http://example.com/a.png"}`)
	write(t, plugins, "file", `{"command":"x","icon":"./icon.png"}`)
	write(t, plugins, "badmodel", `{"command":"x","modelEnv":"M","models":[{"id":"a","icon":"Not A Logo"}]}`)
	got, err := List(dir, plugins)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ProjectAgent{}
	for _, a := range got {
		byID[a.ID] = a
	}
	muse := byID["muse"]
	wantModels := []ProjectAgentModel{{ID: "sonnet", Name: "sonnet", Icon: "claude"}, {ID: "plain", Name: "plain"}}
	if muse.Error != "" || muse.Icon != "https://example.com/muse.png" || !reflect.DeepEqual(muse.Models, wantModels) {
		t.Fatalf("muse %+v", muse)
	}
	for _, name := range []string{"http", "file", "badmodel"} {
		if byID[name].Error == "" || byID[name].Icon != "" {
			t.Fatalf("%s accepted: %+v", name, byID[name])
		}
	}
}
