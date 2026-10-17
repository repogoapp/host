package projectagent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func write(t *testing.T, dir, name, manifest string) {
	t.Helper()
	folder := filepath.Join(dir, Folder, name)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, ManifestFile), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestListShowsEachAgentAndWhyABrokenOneCantRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "helper", `{"name":"Helper","command":"npx","args":["eve","acp"],"cwd":"agent"}`)
	write(t, dir, "plain", `{"command":"gemini"}`)
	write(t, dir, "broken", `{"name":"Broken"}`)
	write(t, dir, "Bad Name", `{"command":"x"}`)
	if err := os.MkdirAll(filepath.Join(dir, Folder, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}

	// From a subfolder, the checkout's agents.
	sub := filepath.Join(dir, "src")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := List(sub)
	if err != nil {
		t.Fatal(err)
	}
	want := []ProjectAgent{
		{ID: "broken", Name: "broken", Error: "agent.json: command is empty"},
		{ID: "helper", Name: "Helper"},
		{ID: "plain", Name: "plain"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("entry %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFindResolvesWhereTheAgentRuns(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "helper", `{"command":"npx","cwd":"agent"}`)
	write(t, dir, "coder", `{"command":"gemini"}`)
	helper, err := Find(dir, "helper")
	if err != nil {
		t.Fatal(err)
	}
	if helper.Workdir() != filepath.Join(dir, Folder, "helper") || helper.Name != "helper" || helper.Fingerprint == "" {
		t.Fatalf("helper %+v", helper)
	}
	coder, err := Find(dir, "coder")
	if err != nil || coder.Workdir() != dir || coder.Cwd != CwdProject {
		t.Fatalf("coder %+v %v", coder, err)
	}
	if coder.Fingerprint == helper.Fingerprint {
		t.Fatal("two manifests share a fingerprint")
	}
	for _, name := range []string{"missing", "../escape", ""} {
		if _, err := Find(dir, name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q: %v", name, err)
		}
	}
	write(t, dir, "odd", `{"command":"x","cwd":"home"}`)
	if _, err := Find(dir, "odd"); err == nil {
		t.Fatal("unknown cwd accepted")
	}
}

func TestListWithoutAgentsIsEmpty(t *testing.T) {
	got, err := List(t.TempDir())
	if err != nil || got != nil {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestModelsListTheirDefaultAndNeedTheirVariable(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "helper", `{"command":"npx","modelEnv":"HELPER_MODEL","models":[{"id":"claude-sonnet-5-5","name":"Sonnet"},{"id":"claude-haiku-5-5"}]}`)
	write(t, dir, "novar", `{"command":"npx","models":[{"id":"x"}]}`)
	write(t, dir, "twice", `{"command":"npx","modelEnv":"M","models":[{"id":"x"},{"id":"x"}]}`)
	got, err := List(dir)
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
	m, err := Find(dir, "helper")
	if err != nil {
		t.Fatal(err)
	}
	for asked, runs := range map[string]string{"": "claude-sonnet-5-5", "claude-haiku-5-5": "claude-haiku-5-5", "gone": "claude-sonnet-5-5"} {
		if got := m.DefaultModel(asked); got != runs {
			t.Fatalf("asked %q runs %q, want %q", asked, got, runs)
		}
	}
}
