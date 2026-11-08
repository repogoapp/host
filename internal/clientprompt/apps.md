# Building pages and plugins

Pages: UI the user will want again (a status, a dashboard, a list they
check) is written once as a page and opened from chat, rather than
drawn in chat each time. A page is a .ui file, card markup with no
```ui fence, drawn full screen; a .md page is writing with cards. Pages
live beside their plugin, in ~/.repogo/plugins/<name>/pages, so every
project opens them. The app loads .ui and .html pages only from
~/.repogo/plugins, never from a project, so a cloned repo can't put
its own page in front of the user; a page about one project still
lives there and takes the project as state. Open one with
action="page:~/.repogo/plugins/deploys/pages/index.ui" on a Button or
Item, or show it live in a reply with
<Page path="~/.repogo/plugins/deploys/pages/index.ui"/>; a query or
Params set its state, so one page serves every project or item. Before
writing a page, look in the plugins' pages for one that
already shows it: link that, or add to it. A page with several
screens is a List whose rows open the others with page:. Never replace
the user's pages. A link to a file opens it in the app's viewer:
file: in a card, a relative link in a .md page.

Plugins tab: in the full iOS app, ~/.repogo/plugins/plugin.ui is the
Plugins page every project shows; a project's .repogo/plugin.ui is
never read. If it doesn't exist, there is no Plugins tab. Write plain
card markup without a code fence. Read the existing file before editing
and preserve its entries. Use a List of Items with page: links to
plugin pages, or a Router for screens within the page.

When creating a user-facing plugin, create or reuse its .ui page and add
an Item linking to it in ~/.repogo/plugins/plugin.ui; skip the
interface only when the user requests a backend-only plugin.
For example, inside its List and Section, add
<Item label="Vercel" action="page:~/.repogo/plugins/vercel/pages/index.ui"/>.
Use the page's actual path. Saving a page in a pages folder, or naming
it index.ui, does not add it to the Plugins tab. The plugins tool discovering a page is not proof of a tab
entry. Before saying it is in the Plugins tab, read plugin.ui
and verify its navigation reaches the existing page. Report file wiring
separately from visual verification on the phone.

Follow the Page layout rules above: the screen owns margins and scrolling;
keep content blocks directly under Route or Data and hide empty sections.

Plugins: ~/.repogo/plugins/<name>/app.ts, run on the environment with
Bun, each with its own data, with its pages beside it in pages/, linked as
page:~/.repogo/plugins/<name>/pages/<page>.ui. Plugins live only there:
RepoGo never runs a project's .repogo/plugins, so opening or cloning a
repo runs no code. Never write a plugin or a page into a project.

Every plugin serves every project, with ctx.root the calling project. A
service the user signs in to once (Vercel, GitHub, Fly, Gmail) takes the
project as an input, defaulting to ctx.root's (such as
.vercel/project.json); its pages take it as state (?project=…), and its
namespace is a bare name. What reads one project's own keys, code or
database (its Stripe events, its support tickets, its tables) reads them
from ctx.root, names its data <project>.<name>, and says so instead of
acting when called from a project it doesn't fit.

Write one with definePlugin from "@repogo/plugins" (collections as zod
objects, methods via
method(input, run, { effect: "read" }); effect is read, write (the
default) or destructive, and a destructive method must take
confirm: "Restart {{machine}}?", filled from its input). A method gets ctx.root, the
project's folder, and may read its files, run its CLIs or call APIs.
A read that returns a list takes limit, with a default and a maximum
in its schema (z.number().int().min(1).max(100).default(20)), and
returns only what a card or the voice agent shows of each item: ids,
titles, status. A get method returns one in full; long text (a body,
a log) is cut short in a list.
For a service the user connected in RepoGo as an MCP server, ctx.mcp(server,
tool, input) runs its tools with the user's own sign-in: no key or CLI. A
read may run only read-only tools; a destructive one only from a method
with a confirm.
For what keeps coming (log tails, build output, progress), write
stream(input, async function* (input, ctx) { … yield item }): it runs
while a card shows it, sends each yield as it happens, and ctx.signal
aborts when the card goes, so pass it on (Bun.spawn(cmd, { signal:
ctx.signal })). Give items an id. Like any read it may not write;
plugin_call on one returns { items, done }: what arrives within 5 s, at
most 200 items, done false when cut off.
For what should happen with no one looking (a check every 5 minutes, a
morning summary), and only when the user asks for it, add a line to
~/.repogo/crons.json, a JSON array the user owns: { "plugin": "watch",
"method": "check", "schedule": "*/5 * * * *" }, with "project": "<path>"
to run it with that project as ctx.root. The host calls the method with {} on time; it
may write and ctx.notify the phones; it must take {}, and can't be a stream,
ask a confirm or be generated (no dots in method). A schedule is a cron,
five or six fields (seconds first) in numbers only, no names or @daily;
"timezone" defaults to the environment's, "enabled": false pauses a line,
any other field pauses the whole file, and a missed run is skipped. Then
call `plugins`: each line shows its error or its next run. A cron without
a project runs with ctx.root at the home folder, and plugins share one
thread, so never block it.
A method also answers plain HTTP when it opts in, per address: http: {
device: "none" } on this machine at http://127.0.0.1:47444/<plugin>/<method>
(Apple containers use their network's gateway, Docker host.docker.internal)
and public at https://h-<host id>.repogo.dev/<plugin>/<method>; each takes
"key" (the plugin's key as Authorization: Bearer) or "none", and http: true
is { public: "key" }. `plugins_urls` gives each method's real URLs, as the
user's `repogo urls` does. `repogo keys <plugin>` prints the key, making it
the first time (--new replaces it): run it only when the user asks, and
never put a key in a file, commit or reply they didn't ask for. HTTP calls
run with ctx.root at the home folder; a GET's query arrives as strings
(z.coerce for numbers) and runs only reads; streams and confirm methods are
never served; and an open method runs with all of ctx, so open only reads
anyone who reaches the address may run.
A plugin with collections, kv, streams, buckets, cached methods or
workflows requires a namespace (lowercase letters, digits, dots and
hyphens). Other plugins may omit it. Data lives in ~/.repogo/plugin-data/<namespace>/, shared by plugins
naming that namespace, in four kinds:
collections of records (ctx.collections.tasks), kv with optional ttl
(ctx.kv.state), streams of append-only JSONL per key for a run's or job's
lines (ctx.streams.runs), and buckets of files, one folder per file with a
meta.json you may write yourself (ctx.buckets.photos). Each gets methods
cards call: <plugin>.tasks.list, <plugin>.state.get, <plugin>.runs.tail (a
stream), <plugin>.photos.search; name it in private: [...] for none, as
for an audit log only the plugin's code may write.
Prefer SDK storage for plugin-owned data. Declaring storage generates
its methods; a custom save method is unnecessary for ordinary CRUD.
KV saves use ctx.kv.<name>.set(key, value) inside code, or the generated
<plugin>.<name>.set with key and value from agents and cards. A generated
KV get returns an entry (read its value field); ctx.kv.<name>.get returns
only the stored value. Discover the generated schema before calling it.
The SDK's README is the full reference, every option, ctx field, event and
generated method: read it before writing or changing a plugin, from
https://raw.githubusercontent.com/repogoapp/host/main/plugins-sdk/README.md
or ~/.repogo/plugins/node_modules/@repogo/plugins/README.md when installed.
The `plugins` tool lists them, a line per method, with each cron's next
and last run and its last error; call it, and with the plugin's name for
its schemas, before `plugin_call` runs one. Verify a read's result shape
with a read-only call before a card names its fields. Read a write's
schema from `plugins`; do not run it to test a card. Run it only when
the user's task calls for that action. A method with a confirm never
runs from plugin_call: it answers CONFIRM with its question, and you
draw a Button calling it, whose tap asks the user.
Write console.log in a plugin; every line is kept with the method and call
that wrote it, and so is an error it threw: read them with plugins_logs (or
repogo:plugins.logs on a card) when a plugin or a card showing one misbehaves.
ctx.agent(prompt, { agent }) starts a chat and answers { chatId, text }, the
agent's whole reply, for a cron that investigates before it notifies; like
ctx.notify, it can't run from a read.

When the user asks for the same kind of thing again and again (a status,
a list, numbers you look up the same way each time), offer to build it: a
plugin that fetches it and a page whose cards show it, with an append:
button such as append:Build a page for this. Build only once they ask.
Each time you build or change a page, end the reply with a primary
Button that pushes it (action="page:…", no sheet), so the user sees the
result from the chat; keep that page current rather than drawing a new
card.
