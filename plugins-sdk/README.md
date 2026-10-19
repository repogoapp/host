# @repogo/plugins

Plugins are small apps that run on your environment, the machine the RepoGo
host runs on. A plugin keeps its own data, does work with your tools and APIs,
and is reached two ways: agents call its methods as tools, and cards on your
phone draw its data live and run its methods when you tap.

- [Quick start](#quick-start)
- [Where plugins live](#where-plugins-live)
- [Methods](#methods): queries, mutations, streams
- [Data](#data): namespaces, collections, kv, streams, buckets
- [Crons](#crons)
- [Logging](#logging)
- [The context](#the-context)
- [Events](#events)
- [How cards and agents reach a plugin](#how-cards-and-agents-reach-a-plugin)
- [Errors and confirmations](#errors-and-confirmations)
- [Testing](#testing)

## Quick start

```ts
// .repogo/plugins/notes/app.ts
import { definePlugin, method, z } from "@repogo/plugins";

export default definePlugin({
  description: "Notes for this project",
  namespace: "myproject.notes",
  collections: { notes: z.object({ text: z.string(), done: z.boolean().default(false) }) },
  methods: {
    open: method(z.object({}), async (_, ctx) => ctx.collections.notes.list({ where: { done: false } }), {
      kind: "query",
      description: "Notes not done yet, newest first",
    }),
    clear: method(z.object({}), async (_, ctx) => {
      for (const note of await ctx.collections.notes.list({ limit: 500 })) await ctx.collections.notes.delete(note.id);
      return null;
    }, { confirm: "Delete every note?" }),
  },
});
```

That's a whole plugin. The host loads it on its next look at the folder (within
a minute; at once when something calls it), and it gets:

- `notes.open` and `notes.clear`, the methods it wrote;
- `notes.notes.list`, `.get`, `.create`, `.update` and `.delete`, generated for its collection;
- a `data.db` in `~/.repogo/plugin-data/myproject.notes/`.

Add the package to `.repogo/plugins/package.json` (`bun add @repogo/plugins`) for
types and editor help. The host runs plugins with its own copy of this package,
the version it shipped with, so a plugin always matches its host. Plugins run on
[Bun](https://bun.sh); install it on the environment.

## Where plugins live

| Folder | For | `ctx.root` |
| --- | --- | --- |
| `<project>/.repogo/plugins/<name>/app.ts` | One project | The project's checkout |
| `~/.repogo/plugins/<name>/app.ts` | Every project (Vercel, Gmail, builds) | The project that called it |

A plugin's name is its folder's: lowercase letters, digits and hyphens. A
project plugin hides an environment one of the same name. Each folder's plugins
run together in one Worker, so a plugin's blocking code stalls the others in its
folder: use async APIs.

Pages beside an environment plugin (`pages/*.ui`, or `pages/*.md`) open from
any project as `~/.repogo/plugins/<name>/pages/<page>.ui`. A
`.ui` page is card markup with no fence; a `.md` page is writing with cards.

## Methods

```ts
method(input, run, { kind?, description?, output?, confirm? })
stream(input, async function* run, { description? })
```

`input` is a zod schema; the call's input is checked against it and defaults
filled in before `run` sees it. `run(input, ctx)` returns the result: anything
JSON can carry.

| Kind | Does | Who runs it |
| --- | --- | --- |
| `query` | Only reads; writing throws | Cards on their own, and again whenever data it read changes |
| `mutation` (the default) | Reads and writes | Only when someone acts: a tap, an agent, a cron, other code |
| `stream` | Only reads; yields items as they happen | A card while it shows it |

Mark every method that only reads as `kind: "query"`: that's what lets a card
show it and keep it current. Give each a `description`; agents read it to choose
a method. `output`, a zod schema of the result, tells agents and cards its
field names.

A **stream** sends each `yield` to the card as it happens, for as long as the
card shows it. `ctx.signal` aborts when the card goes, so hand it to what you
start. Give items an `id`. An agent calling a stream gets what arrives within
5 seconds, at most 200 items.

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

## Data

A plugin declares its data in four kinds, each with a schema, and gets typed
access in `ctx` and generated methods for agents and cards:

| Kind | Holds | `ctx` | On disk |
| --- | --- | --- | --- |
| `collections` | Records: tasks, notes, settings rows | `ctx.collections.<name>` | Rows in `data.db` |
| `kv` | Keys to values, which may expire | `ctx.kv.<name>` | Rows in `data.db` |
| `streams` | Append-only lines per key: a run's log, a job's output | `ctx.streams.<name>` | `streams/<name>/<key>/*.jsonl` |
| `buckets` | Files, each with its meta | `ctx.buckets.<name>` | `buckets/<name>/<id>/` |

Every write is checked against its schema, and every read and write is tracked,
so a card showing a query refreshes when a write touches what it read: in this
plugin, another plugin, or another project.

### Namespaces

`namespace` names where a plugin's data lives: the folder
`~/.repogo/plugin-data/<namespace>/`, holding `data.db` and its streams' and
buckets' files. A plugin that keeps data (any collection, kv, stream or bucket)
must name one, or it fails to load. Plugins naming the same namespace share its
data, in any project, so they must declare the same schemas.

By convention a project's own data is `<project>.<name>` (`myproject.notes`),
and data meant for every project a bare name (`todos`). That's only a
convention: the same namespace is the same data. Data stays put when a project
moves. A plugin with no data has no folder.

### Collections

```ts
collections: { tasks: z.object({ title: z.string(), status: z.enum(["open", "done"]).default("open") }) }
```

```ts
const task = await ctx.collections.tasks.create({ title: "Ship it" }); // { id, title, status, createdAt, updatedAt }
await ctx.collections.tasks.get(task.id);                                // or null
await ctx.collections.tasks.update(task.id, { status: "done" });         // only the fields given change
await ctx.collections.tasks.list({ where: { status: "open" }, orderBy: "updatedAt", order: "desc", limit: 50 });
await ctx.collections.tasks.delete(task.id);
await ctx.collections.tasks.deleteMany({ where: { status: "done" }, offset: 100, limit: 500 }); // done ones past the newest 100, 500 at a time
```

`list` is newest first by default, at most 100 unless `limit` says (up to 500).
`where` matches top-level fields exactly; `null` matches a missing one.
`orderBy` is `id`, `createdAt`, `updatedAt` or one of the record's own fields, and
`offset` passes over that many first. `deleteMany` takes the same options and
deletes exactly what `list` would return, in one transaction, returning how many.

### Kv

```ts
kv: { state: z.object({ step: z.string() }) }
```

```ts
await ctx.kv.state.set("run-42", { step: "build" }, { ttl: "1h" }); // ttl: "30s", "5m", "1h", "7d" or ms
await ctx.kv.state.get("run-42");                                     // { step: "build" }, or null
await ctx.kv.state.entry("run-42");                                   // { key, value, expiresAt, createdAt, updatedAt }
await ctx.kv.state.list({ prefix: "run-", limit: 100 });              // entries in key order
await ctx.kv.state.delete("run-42");
```

The value's schema may be any zod type, not only an object. A key past its
`ttl` reads as missing at once; a sweep every few seconds deletes it and
refreshes cards. Setting a key again without a `ttl` keeps it until deleted.

### Streams

```ts
streams: {
  runs: z.object({ level: z.string(), text: z.string() }),
  audit: { item: z.object({ text: z.string() }), retain: { age: "90d", bytes: 500_000_000 } },
}
```

A stream is append-only JSON lines, split by key: one key per workflow run, job
or day. Each line on disk is `{ offset, at, data }`, in
`streams/<name>/<key>/<first offset>.jsonl`, a new file each megabyte, so you
can `tail` and grep them.

```ts
await ctx.streams.runs.append("run-42", { level: "info", text: "built" }); // { id: "run-42:0", key, offset, at, data }
await ctx.streams.runs.read("run-42", { from: 0, limit: 100 });
for await (const line of ctx.streams.runs.follow("run-42", { signal: ctx.signal })) { /* … */ }
await ctx.streams.runs.end("run-42");   // followers stop; appends fail
await ctx.streams.runs.keys();          // [{ key, lines, bytes, endedAt, updatedAt }], newest first
await ctx.streams.runs.delete("run-42");
```

`follow` replays the last 100 lines (or from `from`, or the last `last`), then
yields each new one, until the key ends or the signal aborts. Keys are letters,
digits, dots, dashes and underscores.

A key is kept 7 days after its last line or its end, and past 50 MB its oldest
files go. `retain` sets `{ age?, bytes? }` per stream, or `"forever"`. Offsets
are never reused: after a crash between writing a line and recording it, the
line is cut off before the next append.

### Buckets

```ts
buckets: { photos: z.object({ caption: z.string().optional(), tags: z.array(z.string()).optional() }) }
```

A bucket keeps files, one folder per file named by its id, holding the file
under its own name and its `meta.json`:

```
buckets/photos/
  01a14cd8-ad7a-7000-8773-aa8bff079ece/
    beach.jpg
    meta.json        {"caption": "Sunset", "folder": "2026"}
```

The folders are the truth. Anyone may add a file by making a folder with the
file in it, and change its meta by writing `meta.json`: an agent with its file
tools, a script, Finder. The host indexes the bucket for `list` and `search` when
the folder changes, and rescans it each minute. Meta may be missing or `{}`;
its one reserved field, `folder`, places the file in a folder of the bucket's
own. Each file reads with a `status`:

| `status` | Meta | `problem` |
| --- | --- | --- |
| `ok` | Matches the schema | null |
| `empty` | None, or `{}` | null |
| `invalid` | Fails the schema; kept as written | Why |
| `malformed` | Not a JSON object, or the folder doesn't hold exactly one file | Why |

```ts
const file = await ctx.buckets.photos.put({ name: "beach.jpg", body: bytes, folder: "2026", meta: { caption: "Sunset" } });
// { id, name, folder, path, size, type, meta, status, problem, revision, createdAt, updatedAt }
await ctx.buckets.photos.get(file.id);
await ctx.buckets.photos.list({ folder: "2026", status: "ok", limit: 100 }); // newest first
await ctx.buckets.photos.search("sunset trip");                            // name, folder and meta; words as prefixes
await ctx.buckets.photos.update(file.id, { tags: ["trip"], caption: null }, { revision: file.revision });
await ctx.buckets.photos.move(file.id, { name: "sunset.jpg", folder: "best" });
const text = await (await ctx.buckets.photos.file(file.id)).text();          // a BunFile
await ctx.buckets.photos.delete(file.id);
```

`body` is a string, bytes or a `Blob` (`Bun.file(path)` copies a file). `path`
is the file on disk. `update` merges into the meta, `null` removing a field, and
checks it against the schema; with `revision`, it fails if the file changed
since you read it. Writes through `ctx` are checked; files written by hand are
indexed with their `status` instead.

### Generated methods

Every collection, kv, stream and bucket gets methods named
`<plugin>.<name>.<method>`, so a plugin that only declares data can be read and
changed by agents and cards with no code of its own:

| Kind | Queries | Mutations | Stream |
| --- | --- | --- | --- |
| Collection | `list`, `get` | `create`, `update`, `delete` (asks) | |
| Kv | `get`, `list` | `set`, `delete` (asks) | |
| Stream | `read`, `keys` | `append`, `end`, `delete` (asks) | `tail` |
| Bucket | `list`, `search`, `get` | `put`, `update`, `move`, `delete` (asks) | |

A bucket's `put` takes `text`, or `from`, an absolute path on the environment to
copy: bytes never travel in a call. A method of your own with the same name as a
generated one fails the plugin's load.

Data named in `private` gets none of these, so cards and agents reach it only
through the plugin's own methods: `private: ["actions", "lines"]` for an audit
log no tap can edit. It hides methods; it isn't a lock. Another plugin naming
the same namespace and store can still read and write it.

## Crons

What runs with no one looking is listed in one file the user owns,
`~/.repogo/crons.json`, not in a plugin's code: a repo you clone can't schedule
itself. Each line names a plugin, one of its methods and a schedule:

```json
[
  { "plugin": "incidents", "method": "watch", "schedule": "*/5 * * * *" },
  { "project": "~/code/shop", "plugin": "backup", "method": "run", "schedule": "0 3 * * *", "timezone": "America/New_York" },
  { "plugin": "digest", "method": "send", "schedule": "0 9 * * 1-5", "enabled": false }
]
```

| Field | What |
| --- | --- |
| `plugin`, `method` | The plugin's folder name and one of its methods, called with `{}` as a tap would |
| `project` | The project whose `.repogo/plugins` holds it, an absolute path or one starting `~/`; left out, `~/.repogo/plugins` |
| `schedule` | A cron: five fields (minute hour day-of-month month day-of-week), or six with seconds first (`*/5 * * * * *`); each `*`, a number, a range, a list or a step |
| `timezone` | Defaults to the environment's |
| `enabled` | `false` keeps the line without running it |

The host reads the file every minute, and again when the agent's `plugins` tool
lists what's there, so an edit shows at once. A folder opened only for its
schedules imports just the plugins they name; the others beside them don't run.
A line removed or disabled is never started again; a run already going finishes.

A job still running isn't started again, a time the environment slept through
is skipped, and nothing is retried. A line naming something missing, a stream, a
method that needs input or asks first, a bad schedule, or the same method twice
says why in `plugins` (each line's `error`), and the others run. A file that
isn't valid JSON pauses every line until it's fixed. Each line also shows
`nextRunAt`, `lastRunAt`, `lastError`, `lastDurationMs` and `runs`; `ctx.plugins()`
has the same for a plugin's own, and a query reading them re-runs around each run.

Schedule something only when the user asks for it. A cron's `ctx.root` is its
project, or the home folder for a plugin in `~/.repogo/plugins`. Plugins in one
folder share a thread: code that blocks it holds up every other cron there, and
the host's log names the crons that were running when it happened.

## Logging

Write `console.log` (or `warn`, `error`, `debug`); nothing else is needed. The
host keeps every line with the plugin, the method and the call that wrote it,
who called (`card`, `agent`, `cron`), and the chat for an agent's call, plus
the arguments as JSON. An error a method throws, or one nothing caught, is
kept too, with its stack. Read them with the agent's `plugin_logs` tool, or on
a card with `<Data method="repogo:plugins.logs">`, which reloads as lines land:
`plugin`, `level` (that level and worse), `method`, `since`, `until`, `query`
(a substring) and `limit` narrow it. The `plugins` tool shows each plugin's
`errors` in the last hour. Lines older than 7 days go, and the oldest hours of
the noisiest plugin once the log passes 2 GB. A line is cut at 8 KB: a payload
belongs in the plugin's own data, not its log.

## The context

Every method, handler and cron gets `ctx`:

| Field | What |
| --- | --- |
| `plugin` | The plugin's name |
| `caller` | Who started the call: `card`, `agent`, `cron`, or `code` |
| `root` | The calling project's checkout |
| `collections`, `kv`, `streams`, `buckets` | The plugin's data |
| `call(method, input)` | Runs a method of a plugin in the same folder; `"open"` is this plugin's own |
| `plugins()` | The plugins in the same folder, with their methods |
| `host(method, params)` | Runs one of the host's methods that agents and cards may call (`git.status`, `chats.list`, `actions.run`, …); others fail with `FORBIDDEN` |
| `mcp(server, tool, input)` | Runs a tool of an MCP server the user connected in RepoGo and switched on for the calling project; see below |
| `notify({ title, body, page?, collapse? })` | Alerts every paired phone; a tap opens `page`, and alerts sharing `collapse` replace each other |
| `agent(prompt, { agent, project?, title?, timeout? })` | Starts a chat with `prompt` and waits for the agent's whole answer: `{ chatId, text }`; see below |
| `emit(event, payload)` | Sends `<plugin>.<event>` to handlers |
| `signal` | Aborts when a stream's card goes |
| `log(...)` | The same as `console.log` |

A query doesn't re-run when what a `ctx.host` method read changes.

### An agent's answer

`ctx.agent` hands a question to Claude or Codex and answers with what it said,
so a cron can watch, investigate and notify with the finding:

```ts
watch: method(z.object({}), async (_, ctx) => {
  const down = await failing(ctx);
  if (!down.length) return null;
  const { text, chatId } = await ctx.agent(`Say in two sentences why ${down[0]} is down.`, { agent: "claude" });
  await ctx.notify({ title: `${down[0]} is down`, body: text, page: `chat:${chatId}` });
  return text;
}),
```

The chat runs in `project` (the calling project unless said), under the
calling chat when an agent called the plugin. It waits up to `timeout`
(10 minutes unless said) and then throws, naming the chat, which keeps
running. The same two host methods are there by hand: `ctx.host("chats.start", …)`
and `ctx.host("chats.wait", …)`, for a chat you want to link to rather than wait for.

### MCP servers

`ctx.mcp` reaches the MCP servers the user connected in RepoGo, by the key
cards name them with, using the user's own sign-in, so a plugin needs no key
or CLI of its own. It answers the tool's structured content, else its text
(parsed when it's JSON):

```ts
customers: method(z.object({}), async (_, ctx) => {
  const list = await ctx.mcp("stripe", "stripe_api_read", { stripe_api_operation_id: "GetCustomers", /* … */ });
  return list.data.map((c) => ({ id: c.id, email: c.email }));
}, { kind: "query" }),
```

A query may run only the server's read-only tools, as a card's `Data` may. A
tool marked destructive runs only from a method with a `confirm`, or one
called from it with `ctx.call`: the user's answer to that question is its
yes. A refusal throws `FORBIDDEN`; a tool's own error throws `INTERNAL` with
its message. Only servers switched on for `ctx.root` answer, so a cron of an
environment plugin, whose root is the home folder, reaches none.

## Events

Handlers in `on` run after the events they name:

```ts
on: {
  "photos.created": async (file, ctx) => { /* caption it */ },
  "tasks.updated": async ({ before, after }, ctx) => { /* … */ },
}
```

| Source | Events |
| --- | --- |
| Collection | `<name>.created` (the record), `<name>.updated` (`{ before, after }`), `<name>.deleted` (the record) |
| Kv | `<name>.created`, `<name>.updated`, `<name>.deleted` (the entry) |
| Bucket | `<name>.created`, `<name>.changed` (the file's bytes), `<name>.metaChanged`, `<name>.deleted` (the file) |
| Any method | `<method>` (`{ input, result }`) |

Changes to a bucket's folder made by hand emit too, once the host sees them.
Events run in memory: none is retried, and a handler that throws is logged.

## How cards and agents reach a plugin

**Agents** use two tools. `plugins` lists every plugin with a line per method
(its kind, whether it asks first, and its input's fields, optional ones ending
in `?`), and every line of `~/.repogo/crons.json` with how it's going.
`plugins` with `plugin` set answers that plugin in full: each method's input
and output JSON Schema and confirm, its `storage` (each bucket's and stream's
folder), the pages that show its methods, and the cron lines naming it.
`plugin_call` runs one. Call a new method once before a card names it.

**Cards** name methods as `plugin:<plugin>.<method>`. A `Data` element runs a
query, or follows a stream, and redraws when its data changes; a `Button` or
`Item` action runs a mutation:

```xml
<Data name="photos" method="plugin:gallery.photos.search">
  <Param name="query" value="{{q}}"/>
  <Each in="photos" as="p"><Item label="{{p.name}}" subtitle="{{p.meta.caption}}"/></Each>
</Data>
<Button label="Mark done" action="plugin:notes.notes.update">
  <Param name="id" value="{{note.id}}"/>
  <Param name="done" value="true" type="boolean"/>
</Button>
```

## Errors and confirmations

Throw `new MethodError(code, message)` for a failure the caller should read:
`VALIDATION`, `NOT_FOUND`, `FORBIDDEN` or `INTERNAL`. Input that fails its
schema answers `VALIDATION` with a message per field. Anything else thrown is
`INTERNAL` with its message.

A method's `confirm` is asked before a card's tap runs it. An agent's
`plugin_call` gets it back as a `CONFIRM` error until it calls again with
`confirmed: true`, once the user agrees. Only `ctx.call` from plugin code skips
it. Give one to anything that can't be undone. `{{field}}` in the question is
filled from the call's input, so it can say what it acts on:
`confirm: "Restart {{machine}}?"`.

## Testing

`@repogo/plugins/testing` runs plugins as the host does, with real data in a
temporary folder; `host` and `mcp` stand in for the host's methods and the
user's MCP servers:

```ts
import { testRuntime } from "@repogo/plugins/testing";
import notes from "./app";

const app = await testRuntime({ notes });
await app.call("notes.add", { text: "hi" });
await app.call("notes.clear", {}, { confirmed: false }); // throws CONFIRM, as an unanswered tap
app.setCrons([{ plugin: "notes", method: "tidy", schedule: "* * * * *" }]); // as a crons.json line would
await app.tickCrons(Date.now() + 60_000);                // runs whatever is due by then
app.notices;                                             // every ctx.notify so far
app.logs;                                                // every console line so far, with its method and caller
await app.close();
```
