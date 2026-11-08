# @repogo/plugins

Plugins are small apps that run on your environment, the machine the RepoGo
host runs on. A plugin keeps its own data, does work with your tools and APIs,
and is reached two ways: agents call its methods as tools, and cards on your
phone draw its data live and run its methods when you tap. A method may also
opt into plain HTTP, for this machine's containers or the public.

- [Quick start](#quick-start)
- [Where plugins live](#where-plugins-live)
- [Methods](#methods): reads, writes, destructive methods, streams, cached reads, [serving over HTTP](#serving-over-http)
- [Data](#data): namespaces, collections, kv, streams, buckets, uploads
- [Workflows](#workflows)
- [Crons](#crons)
- [Logging](#logging)
- [The context](#the-context)
- [Events](#events)
- [How cards and agents reach a plugin](#how-cards-and-agents-reach-a-plugin)
- [Errors and confirmations](#errors-and-confirmations)
- [Testing](#testing)

## Quick start

```ts
// ~/.repogo/plugins/notes/app.ts
import { definePlugin, method, z } from "@repogo/plugins";

export default definePlugin({
  description: "Notes for any project",
  namespace: "notes",
  collections: { notes: z.object({ text: z.string(), done: z.boolean().default(false) }) },
  methods: {
    open: method(z.object({}), async (_, ctx) => ctx.collections.notes.list({ where: { done: false } }), {
      effect: "read",
      description: "Notes not done yet, newest first",
    }),
    clear: method(z.object({}), async (_, ctx) => {
      for (const note of await ctx.collections.notes.list({ limit: 500 })) await ctx.collections.notes.delete(note.id);
      return null;
    }, { effect: "destructive", confirm: "Delete every note?" }),
  },
});
```

That's a whole plugin. The host loads it on its next look at the folder (within
a minute; at once when something calls it), and it gets:

- `notes.open` and `notes.clear`, the methods it wrote;
- `notes.notes.list`, `.get`, `.create`, `.update` and `.delete`, generated for its collection;
- a `data.db` in `~/.repogo/plugin-data/notes/`.

Add the package to `~/.repogo/plugins/package.json` (`bun add @repogo/plugins`) for
types and editor help. The host runs plugins with its own copy of this package,
the version it shipped with, so a plugin always matches its host. Plugins run on
[Bun](https://bun.sh); install it on the environment.

## Where plugins live

| Folder | For | `ctx.root` |
| --- | --- | --- |
| `~/.repogo/plugins/<name>/app.ts` | Every project | The project that called it |

Plugins live only on the environment, in folders the user put there. A project
can't ship plugin code: the host never imports a repo's `.repogo/plugins`, so
cloning or opening a project never runs anything in it. A plugin about one
project (its keys, its database, its plans) is still an environment plugin: it
reads that project from `ctx.root`, and says so instead of acting when it's
called from a project it doesn't fit.

A project ships no pages either: the app loads `.ui` and `.html` pages only
from `~/.repogo/plugins`, so a cloned repo can't put its own page, and the
script an `.html` page runs, in front of the user. A page about one project
lives beside its plugin and takes the project as state (`?project=…`).

A plugin's name is its folder's: lowercase letters, digits and hyphens. The
environment's plugins run together in one Worker, so a plugin's blocking code
stalls the others: use async APIs. They load together too: one that fails to
load (a throw at import, a destructive method without a `confirm`, data without
a `namespace`, a method named like a generated one) stops every plugin in the
folder, and each call answers its error, until it's fixed.

Pages beside an environment plugin (`pages/*.ui`, or `pages/*.md`) open from
any project as `~/.repogo/plugins/<name>/pages/<page>.ui`. A
`.ui` page is card markup with no fence; a `.md` page is writing with cards.

In the full iOS app, `~/.repogo/plugins/plugin.ui` is the Plugins tab in
every project; a project's `.repogo/plugin.ui` is never read. If it doesn't
exist, the tab is absent. It is plain card markup without a code fence. Read and preserve existing
entries, and link to plugin pages with `page:` actions.

A user-facing plugin needs both its saved page and a link from `~/.repogo/plugins/plugin.ui`. Saving a file in a pages folder or naming it `index.ui` does not add it to the tab. Plugin discovery can list a page without a navigation entry; verify `plugin.ui` links to the existing page.

## Methods

```ts
method(input, run, { effect?, description?, output?, confirm?, http? })
cached(input, run, { fresh, shared?, timeout?, description?, output?, http? })
stream(input, async function* run, { description? })
```

`input` is a zod schema; the call's input is checked against it and defaults
filled in before `run` sees it. `run(input, ctx)` returns the result: anything
JSON can carry.

| Effect | Does | Who runs it |
| --- | --- | --- |
| `read` | Only reads; writing throws | Cards on their own, and again whenever data it read changes |
| `write` (the default) | Reads and writes | Only when someone acts: a tap, an agent, a cron, other code |
| `destructive` | Writes what can't be undone; needs a `confirm` | Only after the user answers its question, on a tap |
| `stream()` | A read that yields items as they happen | A card while it shows it |

Mark every method that only reads `effect: "read"`: that's what lets a card
show it and keep it current. Mark what can't be undone (a delete, a send, a
deploy) `effect: "destructive"` with a `confirm` question; a destructive method
without one doesn't load. A method with a `confirm` never runs from an agent's
`plugin_call` or a cron: the agent draws a button, and the user's tap asks. Give each a `description`; agents read it to choose
a method. `output`, a zod schema of the result, tells agents and cards its
field names.

A query that returns a list takes `limit`, with a default and a maximum in its
schema, and returns only what a card or the voice agent shows of each item:
ids, titles, status. A `get` returns one in full, and long text (a body, a log)
is cut short in the list. Answers stay small, so cards draw fast and an agent
or the voice agent reads the whole of one.

```ts
open: method(z.object({ limit: z.number().int().min(1).max(100).default(20) }), async ({ limit }, ctx) =>
  (await ctx.collections.notes.list({ where: { done: false }, limit })).map(({ id, text }) => ({ id, text: text.slice(0, 200) })),
  { effect: "read", description: "Notes not done yet, newest first" }),
```

A **stream** sends each `yield` to the card as it happens, for as long as the
card shows it. `ctx.signal` aborts when the card goes, so hand it to what you
start. Give items an `id`. An agent calling a stream, or `ctx.call` on one,
gets `{ items, done }`: what arrives within 5 seconds, at most 200 items, with
`done` false when it was cut off.

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

A **cached** query is for answers that are slow to get, such as a CLI's list
or another service's API. It answers at once with what it saved last time.
When that answer is older than `fresh`, `run` goes again in the background,
and a card showing it redraws only if the answer changed. With nothing saved
yet, the first call waits for `run`, and its error is the card's.

```ts
projects: cached(z.object({ limit: z.number().int().min(1).max(200).default(100) }),
  async ({ limit }) => listProjects(limit),
  { fresh: "1m", shared: true, description: "Every project the login can see" }),
```

- Answers are kept per method, input and project (`ctx.root`). `shared: true`
  keeps one for every project, for answers that don't depend on it.
- `run` is read-only, like any read's, and has `timeout` (a minute unless it says).
- A background check that fails keeps the saved answer, goes to the plugin's
  logs, and is tried again after another `fresh`.
- Answers live in the namespace's `data.db`, so a plugin with a cached method
  needs a `namespace`. An answer over 1 MB is returned but not saved, and one
  nobody has asked for in 30 days is deleted.

### Serving over HTTP

Cards, agents and crons reach every method. A method answers plain HTTP too
when its `http` option names an address, with who may call it there. `method`
and `cached` take it; a stream or a method with a `confirm` is never served,
since no one is there to watch or answer.

```ts
today: cached(z.object({}), async () => sales(), { fresh: "5m", http: { device: "none" } }),
```

| Address | Where | Reached at |
| --- | --- | --- |
| `device` | This machine's loopback and its Apple containers' gateways, port 47444; Docker Desktop's containers reach it through `host.docker.internal` | `http://127.0.0.1:47444/<plugin>/<method>` |
| `public` | The environment's own name, through the tunnel | `https://h-<host id>.repogo.dev/<plugin>/<method>` |

| Access | Who may call |
| --- | --- |
| `key` | Only a caller sending the plugin's key as `Authorization: Bearer <key>`, which the host checks before the method runs |
| `none` | Anyone who reaches the address. On `device` the network is the boundary; on `public` it's anyone with the URL, since a method doesn't see the request's headers to check a signature |

`http: true` is `{ public: "key" }`. An address a method doesn't name answers
404, as do a method that doesn't exist and a missing or wrong key, so callers
can't learn which plugins are installed.

`repogo urls shop` lists each of a plugin's methods served over HTTP with its
URLs, filled in with this host's id.

Each plugin has one key, for all its methods that take one, at both
addresses. `repogo keys shop` prints it, making it the first time; running it
again shows the same key. `repogo keys shop --new` replaces it, and the old
one stops working at once; `repogo keys shop --delete` leaves the methods
answering no one. RepoGo's tools for agents and a plugin's `ctx.host` can't
reach keys, though anything that runs commands as you can run `repogo keys`,
as it can read your plugins' own secrets. The method itself never sees the key.

A POST's JSON body is the input as it is. A GET's query is the input, as
strings, and only a `read` may be called that way:
link previews and crawlers fetch any URL they're shown, so a write answers
405 to a GET. The answer is the result as JSON, or `{ error, fields }` with
400, 403 or 404 for a `MethodError` with those codes, in its own words. Any
other error, such as a failed `fetch`, answers 500 `Internal server error`,
since its text may hold a token or a path; the host logs the real one. The
device address takes calls only from this machine and its containers'
networks, and refuses a browser's, since any web page can reach loopback.

A body that isn't JSON answers 400, as does one over 1 MB at the device
address (413 at the public one), and the public address answers 429 to a
caller sending too many. A plugin folder that fails to load answers 502. HEAD is a
GET, and a query key given twice keeps its first value. HTTP calls have no
project: `ctx.root` is the home folder. `ctx.caller` is `http` for a public call and `device`
for one at the device address.

When an HTTP caller hangs up, or the public address stops waiting after 60
seconds, `ctx.signal` aborts: hand it to `fetch` and `Bun.spawn` so the work
stops. The public address runs at most 32 calls at once and answers 503 to
another; a call counts until its method returns, so one that ignores the
signal keeps its place until it's done.

A method served over HTTP runs with all of `ctx`: one that starts an agent,
runs an action or calls another method lets its caller do the same. Every
container on this machine reaches every method open to `device`, and so does
every account and app on it. Open only what you'd show anyone who can reach
the address: reads, never a send or a delete.

**A plugin elsewhere that stands in for one here.** `GET /<plugin>` at the
device address lists the methods it serves there with no key, with their
schemas, and `remotePlugin` builds a plugin from it. In a container, a plugin
of the same name whose `app.ts` is only this serves the Mac's plugin, and its
pages, copied or mounted, work unchanged; the code, keys and data stay on the Mac:

```ts
import { remotePlugin } from "@repogo/plugins";
export default await remotePlugin("http://192.168.64.1:47444", "shop");
```

It reads the list when the plugin loads, so it fails to load while the Mac
can't be reached. A page's button for a method the Mac doesn't serve there
fails, as it would for any missing method.

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
buckets' files. A plugin with any collection, kv, stream, bucket, cached method
or workflow must name one, or it fails to load. Plugins naming the same namespace share its
data, in any project, so they must declare the same schemas.

By convention a project's own data is `<project>.<name>` (`myproject.notes`),
and data meant for every project a bare name (`todos`). That's only a
convention: the same namespace is the same data. Data stays put when a project
moves. A plugin without storage, cached methods or workflows needs no data folder.

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

`list` is newest first by default, at most 100 unless `limit` says. The
generated `list` cards and agents call takes `limit` (up to 500), `offset`,
`orderBy` and `order` but no `where`: write a `read` method to filter.
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

### Uploads

A card's `<Upload>` lets the user take a photo, record a video, or pick
photos or files on their phone, and hands them to one of your methods as
`files`. Declare the field with `upload()`:

```ts
import { definePlugin, method, upload, z } from "@repogo/plugins";

ingest: method(z.object({ files: z.array(upload()), month: z.string() }), async ({ files, month }, ctx) => {
  for (const file of files) {
    await ctx.buckets.receipts.put({ body: Bun.file(file.path), name: file.name, folder: month });
  }
  return { added: files.length };
}),
```

Each file is `{ id, name, type, size, path }`. `type` is its MIME type, and
`path` is where the host staged it on the environment. The phone sends the
bytes before the call, encrypted like every call, and the call starts once
they've all arrived. A staged file is deleted a day after its last use, so
copy what you keep: into a bucket, as above, or wherever you like. `files` is
always an array, even for one file.

`upload()` accepts only files the host attached to the running call. Input
written by hand, such as an agent's `plugin_call` with a made-up `path`, fails
validation, and so does a call that sends its own `files` alongside the
upload's. A method the upload's method calls with `ctx.call` may pass the
files on. In tests, `app.call("receipts.ingest", { month: "may" }, { files })`
stands in for the host.

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

## Workflows

> **Experimental.** Workflows are new and may change in ways that need edits
> to your plugins: their methods, statuses and options aren't settled yet.

A workflow keeps progress across host restarts. Define it beside `methods`;
`workflow(input, run, options)` follows the same shape as `method`:

```ts
import { definePlugin, workflow, z } from "@repogo/plugins";

export default definePlugin({
  namespace: "myproject.reports",
  workflows: {
    report: workflow(z.object({ url: z.url() }), async ({ url }, ctx) => {
      const report = await ctx.step.run("fetch", async ({ signal }) => {
        const response = await fetch(url, { signal });
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        return { title: await response.text() };
      }, { kind: "query", attempts: 3, timeout: "30s" });

      await ctx.sleep("5m");

      return await ctx.step.run("save", async () => {
        await ctx.kv.reports.set("latest", report);
        return report;
      });
    }, { timeout: "30d", maxOffline: "14d", retention: "30d" }), // the defaults
  },
  kv: { reports: z.object({ title: z.string() }) },
});
```

For a plugin folder named `reports`, the host generates:

- `reports.report.list({ limit?, status?, cursor? })`: bounded summaries (20 unless `limit` says, at most 100) and a next cursor. A status is `pending`, `running`, `waiting`, `attention`, `completed`, `failed`, `cancelled`, `expired`, `unknown` or `blocked`.
- `reports.report.get({ id })`: one summary, input, result, error and the latest 20 operations (steps and sleeps).
- `reports.report.start({ input, key? })`: persist and queue a run; returns its summary immediately.
- `reports.report.cancel({ id })`: persist cancellation intent and abort active work.
- `reports.report.cancelAll({ status? })`: cancel every unfinished run in the project, or only those in one status; returns `{ cancelled }`.
- `reports.report.retry({ id })`: retry a safely failed query step.
- `reports.report.restart({ id })`: start a run that needs attention over from the beginning, on the current code.

These use the existing plugin methods, `ctx.call`, cards and subscriptions.
The namespace is required. `confirm` on the workflow confirms its start;
it does not preapprove later host actions. An unattended workflow refuses
confirmation-gated plugin methods. The calling project and caller
are preserved. A start `key` deduplicates within that project, plugin and
workflow while its run is retained; reusing it with different input fails.

Await each step and use a unique, stable name. Keep effects inside step callbacks:
the handler runs from the beginning after recovery, and completed callbacks
return their recorded JSON instead of running again. Steps are sequential;
nesting, overlap and reusing a name fail the run. Return JSON, including an
explicit `null` for a step with no result. Large payloads belong in buckets;
a journal record is limited to 1 MiB, a run journal to 64 MiB. `get` omits
input or final results above 16 KiB and sets their truncation flags.

Use `await ctx.sleep("5m")` between steps for a durable wait. No ID is needed:
the runner identifies each sleep by its position among the workflow's operations.
It saves an absolute wake time, releases the executor, and resumes without
repeating completed steps. Loops and consecutive sleeps each get a separate
position. The first recorded duration wins on replay, even when the expression
that supplied it later changes. Durations follow the SDK convention (strings
like `"5m"` or milliseconds), must be positive, and cannot exceed 365 days.

Always await sleep at the workflow level, outside step callbacks; do not catch
its internal waiting signal or detach background workflow operations. Branch
using recorded step results so replay follows the same operation order.
The original run timeout and `maxOffline` still apply during sleep. A sleep
past the run deadline expires at that deadline; an unavailable environment
resumes only when available and within those limits. `list`/`get` show
`waiting` with `nextAttemptAt` as the wake time, and detail rows identify sleeps
with `kind: "sleep"`. Terminal runs have no pending wake time.

`attempts` counts the first invocation and defaults to 1. Only steps declared
`kind: "query"` may automatically retry; they cannot write SDK data or call
mutations. Within query steps, use `ctx.mcp` for read-aware host access;
generic `ctx.host`, notifications and agent starts are refused. A step's `kind`
defaults to `"mutation"`, and `attempts` is at most 100. Query backoff,
`backoff: { initial, factor, max }`, defaults to full jitter with initial `2s`,
factor 2 and maximum `1m`. Import `NonRetryable` and throw it for a permanent failure.
Step `timeout` defaults to `5m`. The callback receives `signal`, `attempt`,
`attempts`, `runId`, `stepId` and an `idempotencyKey` stable across its attempts.
Pass its signal to fetch or subprocess APIs. A timeout aborts cooperatively;
a callback must settle before another attempt can begin.

Mutation steps get one attempt. An interrupted or timed-out mutation becomes
`unknown`: its external effect may have happened, and it is never automatically
repeated. Reconcile that effect before starting new work. Cancellation cannot
roll back remote effects. Manual retry only extends the budget of a failed
query, keeps its completed prefix and original lifetime, and rejects cancelled,
expired, unknown, permanent-failure and invalid-result runs.

### When you edit a workflow

A run always executes the plugin's current code, loaded from its folder as a
method call is; nothing is frozen at start. When a run resumes after an edit,
its finished steps still return their recorded results, so:

- An edit that keeps the recorded steps in the same order, such as a fix
  inside a step or a new step at the end, carries on with the new code.
- An edit the recorded steps can't follow, such as a renamed, removed or
  reordered step, stops the run in `attention`: not finished, and not run
  again on its own. Its `error` says which step it expected. `restart` starts
  it over on the current code, running finished steps again, with the same id,
  key and input and a fresh `timeout`. `cancel` or
  `cancelAll({ status: "attention" })` drops it.

Nested workflow starts from inside a run are not supported.

The host resumes work without an open phone or page. `timeout` is a wall-clock
run lifetime, including waits. `maxOffline` expires work when the environment
or workflow service has been unavailable for that long. A persistent network
outage is also tracked when the environment has no nonlocal network address;
this passive check does not probe external services or prove internet access.
Individual API failures use the step's retry budget. The host records expiry on its next wake.
Shutdown suspends work without recording user cancellation. A finished run's
journal stays until its `retention` passes. Admission refuses new runs above
its storage budget rather than evicting active history. Missing or corrupt
journals block execution. The default environment-wide budget is 512 MiB.
Each unfinished, blocked or attention run reserves 130 MiB for its journal and
one snapshot of its state, including while sleeping. This admits at most three
unfinished runs, fewer when retained history consumes the remaining space;
admission reports storage full. Checkpoint replacement temporarily keeps both
snapshots, so peak disk use can exceed that reservation. The admission budget
is not a free-disk guarantee.

For integration tests, import `testWorkflowRuntime` from
`@repogo/plugins/testing` and pass the fixture's plugins directory. It uses real
temporary storage and a subprocess per run, as the host does. Call the same lifecycle methods,
use `await app.wait("reports.report", id)` to await a terminal/blocked run, and
`await app.close()` afterward. The plain `testRuntime({ plugin })` helper
does not execute durable workflows.


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
| `plugin`, `method` | The plugin's folder name and one of the methods it wrote (not a generated or workflow one), called with `{}` as a tap would |
| `project` | The project the run gets as `ctx.root`, an absolute path or one starting `~/`; left out, the home folder. The plugin is always the environment's |
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
line's project, or the home folder without one. Plugins share a thread: code
that blocks it holds up every other cron, and the host's log names the crons
that were running when it happened.

## Logging

Write `console.log` (or `warn`, `error`, `debug`); nothing else is needed. The
host keeps every line with the plugin, the method and the call that wrote it,
who called (`card`, `agent`, `cron`, `code`, or `http` and `device` for HTTP), and the chat for an agent's call, plus
the arguments as JSON. An error a method throws, or one nothing caught, is
kept too, with its stack. Read them with the agent's `plugins_logs` tool, or on
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
| `caller` | Who started the call: `card`, `agent`, `cron`, `code`, or a request at the public (`http`) or device (`device`) address |
| `root` | The calling project's checkout |
| `collections`, `kv`, `streams`, `buckets` | The plugin's data |
| `call(method, input)` | Runs a method of a plugin in the same folder; `"open"` is this plugin's own |
| `plugins()` | The plugins in the same folder, with their methods |
| `host(method, params)` | Runs one of the host's methods that agents and cards may call (`git.status`, `chats.list`, `actions.run`, …); others fail with `FORBIDDEN`, and one that doesn't exist with `INTERNAL` |
| `mcp(server, tool, input)` | Runs a tool of an MCP server the user connected in RepoGo and switched on for the calling project; see below |
| `notify({ title, body, page?, url?, collapse? })` | Alerts every paired phone; a tap opens `url` (an `https` or `itms-services` link) if set, else `page`, and alerts sharing `collapse` replace each other |
| `activity({ id, title, body?, page?, symbol?, tint?, label?, compactLabel?, progress?, timer?, status? })` | Shows or updates a Live Activity on every paired phone; see below |
| `agent(prompt, { agent, project?, title?, timeout? })` | Starts a chat with `prompt` and waits for the agent's whole answer: `{ chatId, text }`; see below |
| `emit(event, payload)` | Sends `<plugin>.<event>` to handlers |
| `signal` | Aborts when a stream's card goes, or when an HTTP caller leaves before a read or write answers |
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

### Live Activities

`ctx.activity` puts a card on the Lock Screen and in the Dynamic Island.
Its `title`, `body` and `page` are a notice's, and the plugin sets
everything else too. Send the whole state each time: the same `id` updates
it, a field left out takes its default, and `status: "done"` or `"failed"`
ends it with that state, left up for 15 minutes:

```ts
deploy: method(z.object({ project: z.string() }), async ({ project }, ctx) => {
  const steps = ["Build", "Test", "Deploy", "Verify"];
  const page = `~/.repogo/plugins/deploys/pages/index.ui?project=${project}`;
  const show = (current: number, body: string) =>
    ctx.activity({ id: `deploy:${project}`, title: `Deploying ${project}`, body, page,
      symbol: "shippingbox", progress: { kind: "steps", steps, current } });
  await show(0, "Building");
  // …
  await ctx.activity({ id: `deploy:${project}`, title: `${project} is live`, body: "web.app", page,
    status: "done", symbol: "globe", tint: "blue", label: "Live",
    progress: { kind: "steps", steps, current: steps.length } });
}),
```

| Field | Unless said |
| --- | --- |
| `body` | No line under the title |
| `page` | A tap opens the app |
| `symbol` | `circle.dotted`; a check once done, a cross once failed |
| `tint` | White; green once done, red once failed |
| `label` | "2 of 4", "40%", "Done" or "Failed"; `""` hides it |
| `compactLabel` | The `label`, else "2/4" or "40%" |
| `progress` | None. `{ kind: "steps", steps, current }` draws a segment per step, the ones before `current` complete; `{ kind: "bar", value }` a bar from 0 to 1 |
| `timer` | `{ kind: "elapsed" }` from the first start. `{ kind: "countdown", endsAt }` counts down to a Unix ms time and ends nothing; `{ kind: "none" }` hides it |

- The phones get the latest state at most every 5 seconds; a start and an end go at once.
- A state that comes to more than Apple's 4 KB as a push is refused.
- A plugin shows at most two at once; one left without an update for an hour stops counting.
- A query can't call it, as it can't write.
- The text reaches Apple and RepoGo's relay as is, as a `notify` alert's does: keep secrets out of it.

### MCP servers

`ctx.mcp` reaches the MCP servers the user connected in RepoGo, by the key
cards name them with, using the user's own sign-in, so a plugin needs no key
or CLI of its own. It answers the tool's structured content, else its text
(parsed when it's JSON):

```ts
customers: method(z.object({}), async (_, ctx) => {
  const list = await ctx.mcp("stripe", "stripe_api_read", { stripe_api_operation_id: "GetCustomers", /* … */ });
  return list.data.map((c) => ({ id: c.id, email: c.email }));
}, { effect: "read" }),
```

A read may run only the server's read-only tools, as a card's `Data` may. A
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

Changes to a bucket's folder made by hand emit too, once the host sees them;
streams emit none. A plugin's `on` hears only its own events, and runs inside
the call that emitted them, so a slow handler delays that call's answer.
Events run in memory: none is retried, and a handler that throws is logged.

## How cards and agents reach a plugin

**Agents** use two tools. `plugins` lists every plugin with a line per method
(its effect, whether it asks first, and its input's fields, optional ones ending
in `?`), and every line of `~/.repogo/crons.json` with how it's going.
`plugins` with `plugin` set answers that plugin in full: each method's input
and output JSON Schema and confirm, its `storage` (each bucket's and stream's
folder), the pages that show its methods, and the cron lines naming it.
`plugin_call` runs one. Inspect a method's schema before naming it in a card.
Verify a read's fields with a read-only call. Do not execute writes just to
check a card; run them only when the user's task calls for that action.

**Cards** name methods as `plugin:<plugin>.<method>`. A `Data` element runs a
read, or follows a stream, and redraws when its data changes. `Button`,
`Item` and `Swipe` actions call methods; on iOS, `Toggle` can call one on a
user flip. `Upload` calls a plugin method with staged files. Params must match
the discovered input schema. These fragments assume `q` and `note` are supplied
by the enclosing card:

```xml
<Data name="photos" method="plugin:gallery.photos.search">
  <Param name="query" value="{{q}}"/>
  <List>
    <Section label="Photos">
      <Each in="photos" as="p"><Item label="{{p.name}}" subtitle="{{p.meta.caption}}"/></Each>
    </Section>
  </List>
</Data>
<Button label="Mark done" action="plugin:notes.notes.update">
  <Param name="id" value="{{note.id}}"/>
  <Param name="done" value="true" type="boolean"/>
</Button>
```

### Saving from a switch

Prefer SDK storage for plugin-owned data. A storage declaration generates
methods; ordinary saves do not need a custom write method. For example:

```ts
// ~/.repogo/plugins/preferences/app.ts
import { definePlugin, z } from "@repogo/plugins";

export default definePlugin({
  namespace: "myproject.preferences",
  kv: { flags: z.boolean() },
});
```

On iOS this card saves through the generated `preferences.flags.set` method:

```xml
<Toggle name="alerts" label="Notifications" value="false"
        action="plugin:preferences.flags.set">
  <Param name="key" value="alerts"/>
  <Param name="value" value="{{alerts}}" type="boolean"/>
</Toggle>
```

The example starts at a literal `false`; it does not load a saved value.
`Toggle.value` is not a data binding. A page parameter with the declared state
name can supply an initial value; read existing settings before opening the
page with that parameter. User edits then take precedence.

Only a user flip calls the method, with the new value. The switch is disabled
while saving or awaiting confirmation. Failure or cancelled confirmation rolls
back its own edit; a newer edit is preserved. Errors appear beside the control.
An optional local `then` runs after success. Without `action`, a Toggle stays
local. Toggle source actions are not yet implemented by the Android/web renderers.

Inside plugin code, `ctx.kv.flags.set("alerts", true)` saves the same value.
The generated `preferences.flags.get({ key: "alerts" })` returns an entry with
`value` and timestamps, while `ctx.kv.flags.get("alerts")` returns the value alone.

## Errors and confirmations

Throw `new MethodError(code, message, fieldErrors?)` for a failure the caller
should read: `VALIDATION`, `NOT_FOUND`, `FORBIDDEN` or `INTERNAL`.
`fieldErrors` maps a JSON pointer (`"/minutes"`) to its message, and reaches an
HTTP caller as `fields`. Input that fails its schema answers `VALIDATION` with
a message per field. Anything else thrown is
`INTERNAL` with its message. An HTTP caller gets only the first three in their
words; `INTERNAL` reaches it as `Internal server error`
([Serving over HTTP](#serving-over-http)).

A method's `confirm` is asked before a card's tap runs it. An agent's
`plugin_call` never runs it: it gets the question back as a `CONFIRM` error and
draws a button, and the user's tap asks. Ordinary `ctx.call` from plugin code skips
it; an unattended workflow refuses confirmation-gated methods. A `destructive`
method must have one. `{{field}}` in the question is
filled from the call's input, so it can say what it acts on:
`confirm: "Restart {{machine}}?"`.

## Testing

`@repogo/plugins/testing` runs plugins as the host does, with real data in a
temporary folder; `host` and `mcp` stand in for the host's methods and the
user's MCP servers:

```ts
import { testRuntime } from "@repogo/plugins/testing";
import notes from "./app";

const app = await testRuntime({ notes });                 // { root?, mcp?, host? } as a second argument
await app.call("notes.notes.create", { text: "hi" });
await app.call("notes.clear", {}, { confirmed: false }); // throws CONFIRM, as an unanswered tap
app.setCrons([{ plugin: "notes", method: "open", schedule: "* * * * *" }]); // a crons.json line, with root for project
await app.tickCrons(Date.now() + 60_000);                // runs whatever is due by then
app.notices;                                             // every ctx.notify so far
app.logs;                                                // every console line so far, with its method and caller
app.manifest();                                          // the plugins and methods, as the host lists them
await app.close();
```

`call` takes `{ confirmed?, caller?, root?, chat?, files? }`; leaving
`confirmed` out runs a `confirm` method without asking, as `ctx.call` does.
