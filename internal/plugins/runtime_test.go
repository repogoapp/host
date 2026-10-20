package plugins

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/testwait"
)

func TestUnpackWritesTheRuntimeOnceAndFindsItAgain(t *testing.T) {
	dir := t.TempDir()
	entry, err := Unpack(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bundled.ReadFile("runtime/run.js"); err != nil {
		// A dev build carries no runtime: nothing to unpack, plugins run from source.
		if entry != "" {
			t.Fatalf("a build without a runtime unpacked %q", entry)
		}
		return
	}
	if filepath.Base(entry) != "run.js" {
		t.Fatalf("entry %q", entry)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(entry), "worker.js")); err != nil {
		t.Fatalf("no worker beside the entry: %v", err)
	}
	again, err := Unpack(dir)
	if err != nil || again != entry {
		t.Fatalf("second unpack: %q, %v; want %q", again, err, entry)
	}
}

// A release runs a project's plugins from the runtime it carries, with no SDK
// installed in the project: `@repogo/plugins` is the runtime's own copy.
func TestTheBundledRuntimeRunsAProjectsPlugins(t *testing.T) {
	if _, err := bundled.ReadFile("runtime/run.js"); err != nil {
		t.Skip("this build carries no runtime; plugins-sdk's build:runtime makes one")
	}
	if _, err := bunPath(); err != nil {
		t.Skip("bun is not installed")
	}
	home, err := os.MkdirTemp("", "rgr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	runtime, err := Unpack(filepath.Join(home, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(t.Context(), Config{
		Runtime: runtime, Socket: filepath.Join(home, "plugins.sock"), DataDir: filepath.Join(home, "data"),
		Emit: func(device.ID, emit.Event) error { return nil }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	root := project(t, "", map[string]string{"notes": notesPlugin})
	if _, err := s.Call(t.Context(), root, "notes.notes.create", json.RawMessage(`{"text":"a"}`), nil, true, Origin{Caller: CallerCard}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Call(t.Context(), root, "notes.texts", nil, nil, true, Origin{Caller: CallerCard})
	if err != nil || got.Error != nil {
		t.Fatalf("texts: %+v, %v", got, err)
	}
	if texts(t, got.Result)[0] != "a" {
		t.Fatalf("texts = %s", got.Result)
	}
}

func TestBundledRuntimeExecutesADurableWorkflow(t *testing.T) {
	if _, err := bundled.ReadFile("runtime/snapshot-worker.js"); err != nil {
		t.Skip("this build carries no workflow executor")
	}
	if _, err := bunPath(); err != nil {
		t.Skip("bun is not installed")
	}
	home, err := os.MkdirTemp("", "rgw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	entry, err := Unpack(filepath.Join(home, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(t.Context(), Config{
		Runtime: entry, Socket: filepath.Join(home, "plugins.sock"), DataDir: filepath.Join(home, "data"),
		Emit: func(device.ID, emit.Event) error { return nil }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	root := project(t, "", map[string]string{"reports": `import { definePlugin, workflow, z } from "@repogo/plugins";
export default definePlugin({ namespace: "test.reports", workflows: {
 report: workflow(z.object({name:z.string()}), async ({name}, ctx) => {
  const greeting = await ctx.step.run("greet", () => "hello " + name);
  await ctx.sleep("0.02s");
  return await ctx.step.run("result", () => ({greeting,caller:ctx.caller}));
 }),
}});`})
	started, err := s.Call(t.Context(), root, "reports.report.start", json.RawMessage(`{"input":{"name":"world"}}`), nil, true, Origin{Caller: CallerCard})
	if err != nil || started.Error != nil {
		t.Fatalf("start: %+v, %v", started, err)
	}
	var run struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(started.Result, &run); err != nil || run.ID == "" {
		t.Fatalf("run: %s, %v", started.Result, err)
	}
	params, err := json.Marshal(map[string]string{"id": run.ID})
	if err != nil {
		t.Fatal(err)
	}
	testwait.For(t, "the pinned executor to finish", func() bool {
		got, err := s.Call(t.Context(), root, "reports.report.get", params, nil, true, Origin{Caller: CallerCard})
		if err != nil || got.Error != nil {
			t.Fatalf("get: %+v, %v", got, err)
		}
		var result struct {
			Status string `json:"status"`
			Error  any    `json:"error"`
			Result struct {
				Greeting string `json:"greeting"`
				Caller   string `json:"caller"`
			} `json:"result"`
		}
		if err := json.Unmarshal(got.Result, &result); err != nil {
			t.Fatal(err)
		}
		if result.Status == "blocked" || result.Status == "failed" || result.Status == "unknown" {
			t.Fatalf("workflow: %s", got.Result)
		}
		if result.Status != "completed" {
			return false
		}
		if result.Result.Greeting != "hello world" || result.Result.Caller != "card" {
			t.Fatalf("result: %s", got.Result)
		}
		return true
	})
}
