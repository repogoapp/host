package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/testwait"
)

const tickerPlugin = `import { definePlugin, method, z } from "@repogo/plugins";
export default definePlugin({
  namespace: "ticker",
  collections: { ticks: z.object({ at: z.number() }) },
  methods: {
    tick: method(z.object({}), async (_, ctx) => { await ctx.collections.ticks.create({ at: Date.now() }); }),
    count: method(z.object({}), async (_, ctx) => (await ctx.collections.ticks.list()).length, { kind: "query" }),
  },
});
`

// Importing it leaves a file in the project: proof its code ran.
const neighbourPlugin = `import { definePlugin } from "@repogo/plugins";
import { join, resolve } from "node:path";
await Bun.write(join(resolve(import.meta.dir, "..", "..", ".."), "neighbour-ran"), "yes");
export default definePlugin({});
`

// cronService is a service whose crons.json is crons, written as JSON.
func cronService(t *testing.T, sdkDir string, crons any) *Service {
	t.Helper()
	s, _ := newService(t, sdkDir)
	s.cfg.Crons = filepath.Join(t.TempDir(), "crons.json")
	writeCrons(t, s, crons)
	return s
}

func writeCrons(t *testing.T, s *Service, crons any) {
	t.Helper()
	data, ok := crons.(string)
	if !ok {
		raw, err := json.Marshal(crons)
		if err != nil {
			t.Fatal(err)
		}
		data = string(raw)
	}
	if err := os.WriteFile(s.cfg.Crons, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ticks(t *testing.T, s *Service, root string) int {
	t.Helper()
	got, err := s.Call(t.Context(), root, "ticker.count", nil, nil, false, Origin{Caller: CallerCard})
	var n int
	if err != nil || got.Error != nil || json.Unmarshal(got.Result, &n) != nil {
		return -1
	}
	return n
}

// A line in the user's file is all a project plugin needs to run on a
// schedule, and only it loads: the plugin beside it never runs.
func TestACronLineRunsAProjectPluginAndNothingBesideIt(t *testing.T) {
	dir := sdk(t)
	root := project(t, dir, map[string]string{"ticker": tickerPlugin, "neighbour": neighbourPlugin})
	s := cronService(t, dir, []CronEntry{{Project: root, Plugin: "ticker", Method: "tick", Schedule: "* * * * * *"}})

	s.reconcile(t.Context())
	crons := s.Crons()
	if len(crons) != 1 || crons[0].Error != "" || crons[0].NextRunAt == nil {
		t.Fatalf("crons = %+v", crons)
	}
	if !s.cronDirs[filepath.Join(realPath(t, root), ".repogo", "plugins")] {
		t.Fatalf("the project's folder got no schedules: %v", s.cronDirs)
	}
	// ticks() calls the folder, which opens it in full: so check the neighbour first.
	if _, err := os.Stat(filepath.Join(root, "neighbour-ran")); err == nil {
		t.Fatal("the plugin beside the scheduled one ran")
	}
	testwait.For(t, "the cron to run twice", func() bool { return ticks(t, s, root) >= 2 })
}

// A project the file doesn't name runs nothing, however often the host reconciles.
func TestAProjectTheFileDoesntNameRunsNothing(t *testing.T) {
	dir := sdk(t)
	root := project(t, dir, map[string]string{"neighbour": neighbourPlugin})
	s := cronService(t, dir, []CronEntry{})
	s.reconcile(t.Context())
	s.reconcile(t.Context())
	if _, err := os.Stat(filepath.Join(root, "neighbour-ran")); err == nil {
		t.Fatal("a project the file doesn't name ran its code")
	}
	if len(s.Crons()) != 0 {
		t.Fatalf("crons = %+v", s.Crons())
	}
}

// Each bad line says why, and the good ones still run.
func TestBadLinesSayWhyAndTheRestRun(t *testing.T) {
	dir := sdk(t)
	root := project(t, dir, map[string]string{"ticker": tickerPlugin})
	off := false
	s := cronService(t, dir, []CronEntry{
		{Project: root, Plugin: "Ticker", Method: "tick", Schedule: "* * * * *"},
		{Project: "relative/path", Plugin: "ticker", Method: "tick", Schedule: "* * * * *"},
		{Project: root, Plugin: "missing", Method: "tick", Schedule: "* * * * *"},
		{Project: root, Plugin: "ticker", Method: "nope", Schedule: "* * * * *"},
		{Project: root, Plugin: "ticker", Method: "count", Schedule: "every minute"},
		{Project: root, Plugin: "ticker", Method: "tick", Schedule: "* * * * * *"},
		{Project: root, Plugin: "ticker", Method: "tick", Schedule: "*/2 * * * * *"},
		{Project: root, Plugin: "ticker", Method: "count", Schedule: "* * * * *", Enabled: &off},
	})
	s.reconcile(t.Context())
	crons := s.Crons()
	want := []string{"plugin is a plugin's folder name", "project is an absolute path", "there's no plugin missing",
		"isn't one of ticker's methods", "has 2 fields", "", "scheduled twice", ""}
	for i, w := range want {
		if (w == "") != (crons[i].Error == "") || !strings.Contains(crons[i].Error, w) {
			t.Errorf("line %d: error %q, want %q", i, crons[i].Error, w)
		}
	}
	if crons[7].Enabled || crons[7].NextRunAt != nil {
		t.Errorf("a disabled line was scheduled: %+v", crons[7])
	}
	testwait.For(t, "the good line to run", func() bool { return ticks(t, s, root) >= 1 })
}

// A file that can't be read pauses every schedule: a broken edit mustn't keep
// a job running that the user meant to remove.
func TestAnUnreadableFilePausesEverySchedule(t *testing.T) {
	dir := sdk(t)
	root := project(t, dir, map[string]string{"ticker": tickerPlugin})
	s := cronService(t, dir, []CronEntry{{Project: root, Plugin: "ticker", Method: "tick", Schedule: "* * * * *"}})
	s.reconcile(t.Context())
	if len(s.cronDirs) != 1 {
		t.Fatalf("cronDirs = %v", s.cronDirs)
	}
	writeCrons(t, s, `[{"plugin": "ticker", "method": "tick", "schedule": "* * * * *",}]`)
	s.reconcile(t.Context())
	crons := s.Crons()
	if len(crons) != 1 || crons[0].Index != -1 || !strings.Contains(crons[0].Error, "every schedule is paused") {
		t.Fatalf("crons = %+v", crons)
	}
	if len(s.cronDirs) != 0 {
		t.Fatalf("a folder kept its schedules: %v", s.cronDirs)
	}
	// The Worker itself holds none: listing opens the folder, and its manifest shows no crons.
	if crons := manifestCrons(t, s, root, "ticker"); len(crons) != 0 {
		t.Fatalf("the Worker still has schedules: %+v", crons)
	}
	writeCrons(t, s, `[]`+"\n"+`{"plugin": "ticker"}`)
	s.reconcile(t.Context())
	if crons := s.Crons(); len(crons) != 1 || !strings.Contains(crons[0].Error, "goes on after its array") {
		t.Fatalf("content after the array was let through: %+v", crons)
	}
	writeCrons(t, s, `[{"plugin": "ticker", "method": "tick", "schedule": "* * * * *", "every": "minute"}]`)
	s.reconcile(t.Context())
	if crons := s.Crons(); len(crons) != 1 || !strings.Contains(crons[0].Error, "unknown field") {
		t.Fatalf("an unknown field was let through: %+v", crons)
	}
}

// Passes that overlap, as the minute's and a listing's can, end where the file says.
func TestOverlappingReconcilesEndWhereTheFileSays(t *testing.T) {
	dir := sdk(t)
	root := project(t, dir, map[string]string{"ticker": tickerPlugin})
	s := cronService(t, dir, []CronEntry{{Project: root, Plugin: "ticker", Method: "tick", Schedule: "* * * * *"}})
	done := make(chan struct{})
	for range 4 {
		go func() {
			s.reconcile(t.Context())
			done <- struct{}{}
		}()
	}
	writeCrons(t, s, []CronEntry{})
	for range 4 {
		<-done
	}
	s.reconcile(t.Context())
	if len(s.Crons()) != 0 || len(s.cronDirs) != 0 {
		t.Fatalf("crons %+v, dirs %v", s.Crons(), s.cronDirs)
	}
	if crons := manifestCrons(t, s, root, "ticker"); len(crons) != 0 {
		t.Fatalf("the Worker kept a schedule the file dropped: %+v", crons)
	}
}

// manifestCrons is a plugin's crons as its Worker reports them.
func manifestCrons(t *testing.T, s *Service, root, plugin string) []json.RawMessage {
	t.Helper()
	list, err := s.ask(t.Context(), Target{Dir: filepath.Join(root, ".repogo", "plugins")}, "list", Target{Dir: filepath.Join(root, ".repogo", "plugins")})
	if err != nil || list.Error != nil {
		t.Fatalf("list: %+v %v", list.Error, err)
	}
	var got struct {
		Plugins []struct {
			Name  string            `json:"name"`
			Crons []json.RawMessage `json:"crons"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(list.Result, &got); err != nil {
		t.Fatal(err)
	}
	for _, p := range got.Plugins {
		if p.Name == plugin {
			return p.Crons
		}
	}
	t.Fatalf("no plugin %s in %s", plugin, list.Result)
	return nil
}

// The plugins tool shows the file's lines with how each is going, once in the
// list, and in full under the plugin they name.
func TestListShowsTheCrons(t *testing.T) {
	dir := sdk(t)
	root := project(t, dir, map[string]string{"ticker": tickerPlugin})
	s := cronService(t, dir, []CronEntry{{Project: root, Plugin: "ticker", Method: "tick", Schedule: "*/5 * * * *"}})
	for _, plugin := range []string{"", "ticker"} {
		list, err := s.List(t.Context(), root, plugin)
		if err != nil || list.Error != nil {
			t.Fatalf("list %q: %+v %v", plugin, list.Error, err)
		}
		var got struct {
			Crons   []Cron `json:"crons"`
			Plugins []struct {
				Crons []Cron `json:"crons"`
			} `json:"plugins"`
		}
		if err := json.Unmarshal(list.Result, &got); err != nil || len(got.Crons) != 1 || got.Crons[0].Error != "" || got.Crons[0].NextRunAt == nil {
			t.Fatalf("crons in list %q: %s %v", plugin, list.Result, err)
		}
		if len(got.Plugins) > 0 && got.Plugins[0].Crons != nil {
			t.Fatalf("the list repeats the crons under their plugin: %s", list.Result)
		}
	}
}

// A plugin sees who asked: the host names the phone's taps and the agent's calls.
func TestCallsCarryTheirCaller(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	root := project(t, dir, map[string]string{"who": `import { definePlugin, method, z } from "@repogo/plugins";
export default definePlugin({ methods: { caller: method(z.object({}), (_, ctx) => ctx.caller) } });
`})
	for _, caller := range []Caller{CallerCard, CallerAgent} {
		got, err := s.Call(t.Context(), root, "who.caller", nil, nil, true, Origin{Caller: caller})
		if err != nil || got.Error != nil || string(got.Result) != `"`+string(caller)+`"` {
			t.Fatalf("caller %s: %s %+v %v", caller, got.Result, got.Error, err)
		}
	}
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}
