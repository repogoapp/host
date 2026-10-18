package plugins

import (
	"encoding/json"
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
  crons: [{ method: "tick", schedule: "* * * * * *" }],
});
`

// Keeping a project's folder open is all a cron needs: it runs with no card or agent calling it.
func TestKeepRunsAProjectsCronsWithNoOneCalling(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	root := project(t, dir, map[string]string{"ticker": tickerPlugin})
	other := t.TempDir()

	s.keep(t.Context(), []string{root, other})

	testwait.For(t, "the cron to run twice", func() bool {
		got, err := s.Call(t.Context(), root, "ticker.count", nil, false)
		var n int
		return err == nil && got.Error == nil && json.Unmarshal(got.Result, &n) == nil && n >= 2
	})
	list, err := s.List(t.Context(), root)
	if err != nil || list.Error != nil || !strings.Contains(string(list.Result), `"schedule":"* * * * * *"`) {
		t.Fatalf("the manifest doesn't list the cron: %s %+v %v", list.Result, list.Error, err)
	}
}

// A cron that could never run keeps the folder from loading, and says why.
func TestABadCronFailsTheLoad(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	root := project(t, dir, map[string]string{"ticker": strings.Replace(tickerPlugin, `"* * * * * *"`, `"every minute"`, 1)})

	list, err := s.List(t.Context(), root)
	if err != nil || list.Error == nil || !strings.Contains(list.Error.Message, "has 2 fields") {
		t.Fatalf("list = %s %+v %v", list.Result, list.Error, err)
	}
}
