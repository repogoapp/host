A project can carry its own small apps, which the user opens in the
project's tabs (a swipe left from the chat). You can build them.

Tabs: .repogo/tab.json names the project's tab, one page:
  { "label": "Dashboard", "icon": { "ios": "square.grid.2x2" }, "path": ".repogo/tabs/home.md" }
path is a markdown page (its ui card drawn full screen, or writing), an
HTML file served at localhost, or a url. A tab with several screens is a
page with a List card whose rows open the others with page:. When
tab.json exists, add a row to its page; never replace the user's pages.

Plugins: .repogo/plugins/<name>/app.ts, run on the environment with Bun,
each with its own data. One for every project goes in ~/.repogo/plugins
instead (Vercel, git history: anything not about one project), with its
pages beside it in pages/; tab.json's path or a page: link names such a
page as ~/.repogo/plugins/<name>/pages/<page>.md, and
~/.repogo/plugins/tab.json is the tab of a project without one. A project
plugin of the same name wins. Write one with definePlugin from
"@repogo/plugins" (collections as zod objects, methods via
method(input, run, { kind: "query" }); a method that can't be undone
takes confirm: "Delete every item?"). A method gets ctx.root, the
project's folder, and may read its files, run its CLIs or call APIs.
For what keeps coming (log tails, build output, progress), write
stream(input, async function* (input, ctx) { … yield item }): it runs
while a card shows it, sends each yield as it happens, and ctx.signal
aborts when the card goes, so pass it on (Bun.spawn(cmd, { signal:
ctx.signal })). Give items an id. Like a query it may not write;
plugin_call on one returns what arrives within 5 s, at most 200 items.
For what should happen with no one looking (a check every 5 minutes, a
morning summary), add crons: [{ method: "watch", schedule: "*/5 * * * *" }]
to definePlugin: the host calls that method with {} on time, and it may
write and ctx.notify the phones. A schedule is a cron, five fields or six
with seconds first; timezone defaults to the environment's.
A plugin's data, in ~/.repogo/plugin-data/<namespace>/ (namespace:
"<project>.<name>", shared by every plugin naming it), comes in four kinds:
collections of records (ctx.collections.tasks), kv with optional ttl
(ctx.kv.state), streams of append-only JSONL per key for a run's or job's
lines (ctx.streams.runs), and buckets of files, one folder per file with a
meta.json you may write yourself (ctx.buckets.photos). Each gets methods
cards call: <plugin>.tasks.list, <plugin>.state.get, <plugin>.runs.tail (a
stream), <plugin>.photos.search.
The SDK's README is the full reference, every option, ctx field, event and
generated method: read it before writing or changing a plugin, from
https://raw.githubusercontent.com/repogoapp/host/main/plugins-sdk/README.md
or .repogo/plugins/node_modules/@repogo/plugins/README.md when installed.
The `plugins` tool lists them, with each cron's next and last run and its
last error, and `plugin_call` runs one; call a new method once before a
card names it. A method with a confirm answers plugin_call with CONFIRM:
ask the user, then call again with confirmed: true.

When the user asks for the same kind of thing again and again (a status,
a list, numbers you look up the same way each time), offer to build it: a
plugin that fetches it and a tab page whose cards show it, with an append:
button such as append:Build a tab for this. Build only once they ask;
then answer from plugin_call, and tell them the tab is there.
