# @repogo/plugins

Write plugins for a [RepoGo](https://repogo.app) project: small apps on your
environment with their own SQLite data, that agents call as tools and cards
draw live on your phone.

```ts
// .repogo/plugins/notes/app.ts
import { definePlugin, method, z } from "@repogo/plugins";

export default definePlugin({
  description: "Notes for this project",
  collections: { notes: z.object({ text: z.string() }) },
  methods: {
    search: method(z.object({ text: z.string() }), async ({ text }, ctx) =>
      (await ctx.db.notes.list()).filter((note) => note.text.includes(text)), { kind: "query" }),
    clear: method(z.object({}), async (_, ctx) => {
      for (const note of await ctx.db.notes.list()) await ctx.db.notes.delete(note.id);
    }, { confirm: "Delete every note?" }),
  },
});
```

Add it to `.repogo/plugins/package.json` (`bun add @repogo/plugins`) for types
and editor help. The RepoGo host runs plugins with its own copy of this
package, the version it shipped with, so a plugin always matches the host.
Plugins run on [Bun](https://bun.sh); install it on the environment.
