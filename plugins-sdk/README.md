# @repogo/plugins

Write plugins for a [RepoGo](https://repogo.app) project: small apps on your
environment with their own SQLite data, that agents call as tools and cards
draw live on your phone.

```ts
// .repogo/plugins/notes/app.ts
import { definePlugin, method, z } from "@repogo/plugins";

export default definePlugin({
  description: "Notes for this project",
  db: "myproject.notes",
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

A `stream` sends items as they happen, for as long as a card shows it. Each
`yield` is one item; `ctx.signal` aborts when the card goes, so hand it to what
you start:

```ts
tail: stream(z.object({ file: z.string() }), async function* ({ file }, ctx) {
  const tail = Bun.spawn(["tail", "-f", file], { signal: ctx.signal });
  let partial = "";
  for await (const chunk of tail.stdout.pipeThrough(new TextDecoderStream())) {
    const lines = (partial + chunk).split("\n");
    partial = lines.pop()!;
    for (const line of lines) yield { id: crypto.randomUUID(), text: line };
  }
}),
```

`db` names the SQLite file the collections live in, `~/.repogo/plugin-data/<db>.db`
on the environment; left out, it is the plugin's folder name. Name a project's own
data `<project>.<name>`. A bare name (`todos`) is shared by every plugin that
names it, in any project, so they must agree on their collections' schemas.
Data stays in place when the project moves. A plugin without collections has no file.

A plugin for every project goes in `~/.repogo/plugins/<name>/app.ts` instead
of a project's folder: the same shape, run with `ctx.root` set to whichever
project calls it. A project plugin of the same name takes precedence. Pages
beside it (`pages/*.md`) are reachable from any project as
`~/.repogo/plugins/<name>/pages/<page>.md`, and `~/.repogo/plugins/tab.json`
lists the pages every project's tab shows.

A method may alert the user's phones: `ctx.notify({ title, body, page })`, where
`page` is a page target a tap opens (`".repogo/tabs/vercel.md?project=a"`), and
`collapse` lets a repeated alert replace itself.

Add it to `.repogo/plugins/package.json` (`bun add @repogo/plugins`) for types
and editor help. The RepoGo host runs plugins with its own copy of this
package, the version it shipped with, so a plugin always matches the host.
Plugins run on [Bun](https://bun.sh); install it on the environment.
