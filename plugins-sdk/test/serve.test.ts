import { afterEach, expect, test } from "bun:test";
import { mkdir, mkdtemp, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import type { DataPush, HostCall, LogPush, NotifyPush, Reply } from "../src/protocol";
import { serve } from "../src/serve";

// Re-runs settle fast here; the server's default is 100 ms.
process.env.REPOGO_PLUGINS_SETTLE_MS = "10";

const sdk = resolve(import.meta.dir, "..");

const notesApp = `import { definePlugin, method, z } from "@repogo/plugins";
export default definePlugin({
  namespace: "notes",
  collections: {
    notes: z.object({ text: z.string() }),
    tags: z.object({ name: z.string() }),
  },
  methods: {
    texts: method(z.object({}), async (_, ctx) => (await ctx.collections.notes.list({ order: "asc" })).map((n) => n.text), { kind: "query" }),
    root: method(z.object({}), (_, ctx) => ctx.root, { kind: "query" }),
    slowTexts: method(z.object({ status: z.string().default("") }), async (_, ctx) => {
      await Bun.sleep(150);
      return (await ctx.collections.notes.list({ order: "asc" })).map((n) => n.text);
    }, { kind: "query" }),
    byPrefix: method(z.object({ prefix: z.string() }), async ({ prefix }, ctx) =>
      (await ctx.collections.notes.list({ order: "asc" })).map((n) => n.text).filter((t) => t.startsWith(prefix)), { kind: "query" }),
  },
});
`;

/** A project checkout with the SDK linked in and these plugins written. */
async function checkout(plugins: Record<string, string>) {
  const root = await mkdtemp(join(tmpdir(), "repogo-checkout-"));
  const dir = join(root, ".repogo", "plugins");
  await mkdir(join(dir, "node_modules", "@repogo"), { recursive: true });
  await symlink(sdk, join(dir, "node_modules", "@repogo", "plugins"));
  for (const [name, source] of Object.entries(plugins)) {
    await mkdir(join(dir, name), { recursive: true });
    await writeFile(join(dir, name, "app.ts"), source);
  }
  return { root, dir };
}

type Client = {
  request: (method: string, params: unknown) => Promise<Reply>;
  /** Answers a request the server made, as the host answers a host call. */
  answer: (reply: Reply) => void;
  pushes: (DataPush | NotifyPush | HostCall | LogPush)[];
  /** Resolves with the next push that matches, or fails after a second. */
  nextPush: (match: (push: DataPush | NotifyPush | HostCall | LogPush) => boolean) => Promise<DataPush | NotifyPush | HostCall | LogPush>;
  close: () => void;
};

async function connect(socket: string): Promise<Client> {
  let nextID = 0;
  let inbox = "";
  const waiting = new Map<number, (reply: Reply) => void>();
  const pushes: (DataPush | NotifyPush | HostCall | LogPush)[] = [];
  const watchers: (() => void)[] = [];
  const connection = await Bun.connect({
    unix: socket,
    socket: {
      data(_, chunk) {
        inbox += chunk.toString();
        let newline: number;
        while ((newline = inbox.indexOf("\n")) >= 0) {
          const message = JSON.parse(inbox.slice(0, newline));
          inbox = inbox.slice(newline + 1);
          if ("method" in message) {
            pushes.push(message);
            for (const watch of watchers) watch();
          } else {
            waiting.get(message.id)?.(message);
            waiting.delete(message.id);
          }
        }
      },
    },
  });
  return {
    pushes,
    request(method, params) {
      const id = ++nextID;
      return new Promise((resolve) => {
        waiting.set(id, resolve);
        connection.write(JSON.stringify({ id, method, params }) + "\n");
      });
    },
    nextPush(match) {
      const seen = pushes.length;
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error("no push within a second")), 1000);
        const watch = () => {
          const push = pushes.slice(seen).find(match);
          if (!push) return;
          clearTimeout(timer);
          resolve(push);
        };
        watchers.push(watch);
        watch();
      });
    },
    answer: (reply) => connection.write(JSON.stringify(reply) + "\n"),
    close: () => connection.end(),
  };
}

let stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops) stop();
  stops = [];
});

async function start() {
  const home = await mkdtemp(join(tmpdir(), "repogo-serve-"));
  const socket = join(home, "plugins.sock");
  const server = await serve({ socket, dataDir: join(home, "data") });
  const client = await connect(socket);
  stops.push(() => client.close(), () => server.stop());
  return { socket, client, dataDir: join(home, "data") };
}

const result = (reply: Reply) => {
  if ("error" in reply) throw new Error(`${reply.error.code}: ${reply.error.message}`);
  return reply.result as any;
};

test("the socket is the owner's alone", async () => {
  const { socket } = await start();
  expect((await stat(socket)).mode & 0o777).toBe(0o600);
});

test("list and call reach the checkout's plugins; data is one folder per namespace", async () => {
  const { client, dataDir } = await start();
  const { root, dir } = await checkout({ notes: notesApp });
  const target = { dir };

  const manifest = result(await client.request("list", target));
  expect(manifest.plugins.map((plugin: { name: string }) => plugin.name)).toEqual(["notes"]);

  result(await client.request("call", { ...target, name: "notes.notes.create", input: { text: "a" } }));
  expect(result(await client.request("call", { ...target, name: "notes.texts" }))).toEqual(["a"]);
  expect(result(await client.request("call", { ...target, name: "notes.root" }))).toBe(root);
  expect((await stat(join(dataDir, "notes", "data.db"))).isFile()).toBe(true);
});

test("a subscription gets a push when a write changes its result, and nothing otherwise", async () => {
  const { client } = await start();
  const { dir } = await checkout({ notes: notesApp });
  const target = { dir };

  const first = result(await client.request("subscribe", { ...target, subscription: "t1", name: "notes.texts" }));
  expect(first).toEqual({ subscription: first.subscription, revision: 1, result: [] });

  const pushed = client.nextPush((push) => push.params.subscription === first.subscription);
  result(await client.request("call", { ...target, name: "notes.notes.create", input: { text: "a" } }));
  expect((await pushed).params).toEqual({ subscription: first.subscription, revision: 2, result: ["a"] });

  // A write to another collection doesn't touch what the query read.
  result(await client.request("call", { ...target, name: "notes.tags.create", input: { name: "x" } }));
  await Bun.sleep(100);
  expect(client.pushes).toHaveLength(1);
});

test("a write in one checkout reaches the other checkout's subscriptions", async () => {
  const { client } = await start();
  const main = await checkout({ notes: notesApp });
  const worktree = await checkout({ notes: notesApp });

  const subscribed = result(await client.request("subscribe", { dir: main.dir, subscription: "t2", name: "notes.texts" }));
  const pushed = client.nextPush((push) => push.params.subscription === subscribed.subscription);
  result(await client.request("call", { dir: worktree.dir, name: "notes.notes.create", input: { text: "from the worktree" } }));
  expect((await pushed).params).toMatchObject({ revision: 2, result: ["from the worktree"] });
});

test("plugins in two checkouts that name the same namespace share it, and a write in one refreshes the other", async () => {
  const { client, dataDir } = await start();
  const todos = notesApp.replace('namespace: "notes"', 'namespace: "todos"');
  const one = await checkout({ notes: todos });
  const two = await checkout({ list: todos });

  const subscribed = result(await client.request("subscribe", { dir: one.dir, subscription: "t2b", name: "notes.texts" }));
  const pushed = client.nextPush((push) => push.params.subscription === subscribed.subscription);
  result(await client.request("call", { dir: two.dir, name: "list.notes.create", input: { text: "shared" } }));
  expect((await pushed).params).toMatchObject({ revision: 2, result: ["shared"] });
  expect((await stat(join(dataDir, "todos", "data.db"))).isFile()).toBe(true);
});

test("a call's root is the caller's project; the folder's own checkout when left out", async () => {
  const { client } = await start();
  const { root, dir } = await checkout({ notes: notesApp });
  expect(result(await client.request("call", { dir, name: "notes.root" }))).toBe(root);
  expect(result(await client.request("call", { dir, root: "/tmp/elsewhere", name: "notes.root" }))).toBe("/tmp/elsewhere");
  const subscribed = result(await client.request("subscribe", { dir, root: "/tmp/elsewhere", subscription: "r1", name: "notes.root" }));
  expect(subscribed.result).toBe("/tmp/elsewhere");
});

test("a plugin's notify reaches the host as a push, naming the plugin and the project", async () => {
  const { client } = await start();
  const alerting = notesApp.replace("methods: {", `methods: {
    alert: method(z.object({}), (_, ctx) => { ctx.notify({ title: "Build failed", body: "see logs", page: "x.md?a=1" }); return null; }),`);
  const { root, dir } = await checkout({ notes: alerting });
  const pushed = client.nextPush((push) => push.method === "notify");
  result(await client.request("call", { dir, name: "notes.alert" }));
  expect((await pushed).params).toEqual({ title: "Build failed", body: "see logs", page: "x.md?a=1", plugin: "notes", root });
});

test("a plugin's ctx.host goes to the host, naming the plugin, and gets its answer or its error", async () => {
  const { client } = await start();
  const hosting = notesApp.replace("methods: {", `methods: {
    status: method(z.object({}), (_, ctx) => ctx.host("git.status", { path: ctx.root })),
    refused: method(z.object({}), (_, ctx) => ctx.host("files.write", {}).catch((error) => error.code)),`);
  const { root, dir } = await checkout({ notes: hosting });

  const asked = client.nextPush((push) => push.method === "host");
  const status = client.request("call", { dir, name: "notes.status" });
  const call = (await asked) as HostCall;
  expect(call.params).toEqual({ plugin: "notes", method: "git.status", params: { path: root } });
  client.answer({ id: call.id, result: { branch: "main" } });
  expect(result(await status)).toEqual({ branch: "main" });

  const askedAgain = client.nextPush((push) => push.method === "host" && push.params.method === "files.write");
  const refused = client.request("call", { dir, name: "notes.refused" });
  client.answer({ id: ((await askedAgain) as HostCall).id, error: { code: "FORBIDDEN", message: "plugins may only call the methods agents and cards can" } });
  expect(result(await refused)).toBe("FORBIDDEN");
});

test("a reload runs the new code and pushes what changed", async () => {
  const { client } = await start();
  const { dir } = await checkout({ notes: notesApp });
  const target = { dir };
  const subscribed = result(await client.request("subscribe", { ...target, subscription: "t3", name: "notes.texts" }));

  await writeFile(join(dir, "notes", "app.ts"), notesApp.replace("(n) => n.text", "(n) => n.text.toUpperCase()").replace("ctx.root", '"new"'));
  result(await client.request("call", { ...target, name: "notes.notes.create", input: { text: "a" } }));
  await client.nextPush((push) => push.params.subscription === subscribed.subscription);

  const reloaded = client.nextPush((push) => push.params.subscription === subscribed.subscription && push.params.revision === 3);
  result(await client.request("reload", { dir }));
  expect((await reloaded).params).toMatchObject({ result: ["A"] });
  expect(result(await client.request("call", { ...target, name: "notes.root" }))).toBe("new");
});

test("after unsubscribe a write pushes nothing", async () => {
  const { client } = await start();
  const { dir } = await checkout({ notes: notesApp });
  const target = { dir };
  const subscribed = result(await client.request("subscribe", { ...target, subscription: "t4", name: "notes.texts" }));
  result(await client.request("unsubscribe", { subscription: subscribed.subscription }));
  result(await client.request("call", { ...target, name: "notes.notes.create", input: { text: "a" } }));
  await Bun.sleep(100);
  expect(client.pushes).toHaveLength(0);
});

test("a subscription only runs queries", async () => {
  const { client } = await start();
  const { dir } = await checkout({ notes: notesApp });
  const reply = await client.request("subscribe", { dir, subscription: "t5", name: "notes.notes.create", input: { text: "a" } });
  expect(reply).toMatchObject({ error: { code: "FORBIDDEN" } });
});

test("plugins that fail to load answer every request with why", async () => {
  const { client } = await start();
  const { dir } = await checkout({ broken: "export default {{{" });
  const reply = await client.request("list", { dir });
  expect(reply).toMatchObject({ error: { code: "INTERNAL" } });
  expect("error" in reply && reply.error.message).toContain("failed to load");
});

test("a malformed request is refused with its fields", async () => {
  const { client } = await start();
  const reply = await client.request("call", { dir: "relative", name: "" });
  expect(reply).toMatchObject({ error: { code: "VALIDATION" } });
  expect("error" in reply && Object.keys(reply.error.fieldErrors ?? {})).toContain("/params/dir");
});

test("subscribing again under an id replaces it: one push per change", async () => {
  const { client } = await start();
  const { dir } = await checkout({ notes: notesApp });
  const target = { dir };
  result(await client.request("subscribe", { ...target, subscription: "r1", name: "notes.texts" }));
  const again = result(await client.request("subscribe", { ...target, subscription: "r1", name: "notes.texts" }));
  expect(again.revision).toBe(1);

  const pushed = client.nextPush((push) => push.params.subscription === "r1");
  result(await client.request("call", { ...target, name: "notes.notes.create", input: { text: "a" } }));
  await pushed;
  await Bun.sleep(100);
  expect(client.pushes.filter((push) => push.params.subscription === "r1")).toHaveLength(1);
});

test("an unsubscribe during the first run means it never starts", async () => {
  const { client } = await start();
  const { dir } = await checkout({ notes: notesApp });
  const target = { dir };
  const subscribing = client.request("subscribe", { ...target, subscription: "slow", name: "notes.slowTexts" });
  result(await client.request("unsubscribe", { subscription: "slow" }));
  expect(await subscribing).toMatchObject({ error: { code: "INTERNAL" } });

  result(await client.request("call", { ...target, name: "notes.notes.create", input: { text: "a" } }));
  await Bun.sleep(400);
  expect(client.pushes).toHaveLength(0);
});

test("rapid input changes under one id push only for the latest input", async () => {
  const { client } = await start();
  const { dir } = await checkout({ notes: notesApp });
  const target = { dir };
  const a = client.request("subscribe", { ...target, subscription: "f", name: "notes.byPrefix", input: { prefix: "a" } });
  const b = client.request("subscribe", { ...target, subscription: "f", name: "notes.byPrefix", input: { prefix: "b" } });
  const again = client.request("subscribe", { ...target, subscription: "f", name: "notes.byPrefix", input: { prefix: "a" } });
  await Promise.all([a, b]);
  expect(result(await again).result).toEqual([]);

  const pushed = client.nextPush((push) => push.params.subscription === "f");
  result(await client.request("call", { ...target, name: "notes.notes.create", input: { text: "b-only" } }));
  result(await client.request("call", { ...target, name: "notes.notes.create", input: { text: "apple" } }));
  expect((await pushed).params).toMatchObject({ result: ["apple"] });
  await Bun.sleep(150);
  for (const push of client.pushes) expect(push.params).toMatchObject({ result: ["apple"] });
});

const streamsApp = (pidFile: string) => `import { definePlugin, stream, z } from "@repogo/plugins";
export default definePlugin({
  methods: {
    ticks: stream(z.object({ count: z.number() }), async function* ({ count }) {
      for (let i = 1; i <= count; i++) yield i;
    }),
    broken: stream(z.object({}), async function* () {
      yield "first";
      throw new Error("source went away");
    }),
    sleeper: stream(z.object({}), async function* (_, ctx) {
      const child = Bun.spawn(["sleep", "30"], { signal: ctx.signal });
      await Bun.write(${JSON.stringify(pidFile)}, String(child.pid));
      yield "started";
      await child.exited;
    }),
  },
});
`;

/** Resolves once `pid` is gone, or fails after a second. */
async function exited(pid: number) {
  for (let i = 0; i < 100; i++) {
    try { process.kill(pid, 0); } catch { return; }
    await Bun.sleep(10);
  }
  throw new Error(`${pid} still running`);
}

test("a stream answers empty, then pushes its items and its end", async () => {
  const { client, socket } = await start();
  const { dir } = await checkout({ feed: streamsApp(join(socket, "..", "pid")) });
  const ended = client.nextPush((push) => "done" in push.params);
  expect(result(await client.request("subscribe", { dir, subscription: "s1", name: "feed.ticks", input: { count: 3 } })))
    .toEqual({ subscription: "s1", revision: 1, result: [], stream: true });
  expect((await ended).params).toEqual({ subscription: "s1", revision: 2, items: [1, 2, 3], done: true });
});

test("a stream that throws ends with its error, after what it sent", async () => {
  const { client, socket } = await start();
  const { dir } = await checkout({ feed: streamsApp(join(socket, "..", "pid")) });
  const ended = client.nextPush((push) => "done" in push.params);
  result(await client.request("subscribe", { dir, subscription: "s2", name: "feed.broken" }));
  expect((await ended).params).toMatchObject({ items: ["first"], done: true, error: { message: "source went away" } });
});

test("unsubscribing aborts a stream, and what it started stops", async () => {
  const { client, socket } = await start();
  const pidFile = join(socket, "..", "pid");
  const { dir } = await checkout({ feed: streamsApp(pidFile) });
  const started = client.nextPush((push) => "items" in push.params);
  result(await client.request("subscribe", { dir, subscription: "s3", name: "feed.sleeper" }));
  await started;
  const pid = Number(await Bun.file(pidFile).text());
  result(await client.request("unsubscribe", { subscription: "s3" }));
  await exited(pid);
});

test("a reload stops a stream's process and starts it over from empty", async () => {
  const { client, socket } = await start();
  const pidFile = join(socket, "..", "pid");
  const { dir } = await checkout({ feed: streamsApp(pidFile) });
  const started = client.nextPush((push) => "items" in push.params);
  result(await client.request("subscribe", { dir, subscription: "s4", name: "feed.sleeper" }));
  await started;
  const pid = Number(await Bun.file(pidFile).text());
  const restarted = client.nextPush((push) => "result" in push.params);
  result(await client.request("reload", { dir }));
  expect((await restarted).params).toEqual({ subscription: "s4", revision: 3, result: [] });
  await exited(pid);
});

const clipsApp = `import { definePlugin, z } from "@repogo/plugins";
export default definePlugin({
  namespace: "clips",
  buckets: { files: z.object({ note: z.string().optional() }) },
  streams: { log: z.object({ text: z.string() }) },
});
`;

test("a file dropped into a bucket refreshes a card's list, through the Worker's watcher", async () => {
  const { client } = await start();
  const { dir } = await checkout({ clips: clipsApp });
  const manifest = result(await client.request("list", { dir }));
  const bucket = manifest.plugins[0].storage.buckets[0].path as string;

  const subscribed = result(await client.request("subscribe", { dir, subscription: "b1", name: "clips.files.list" }));
  expect(subscribed.result).toEqual([]);
  const pushed = client.nextPush((push) => push.params.subscription === subscribed.subscription);
  await mkdir(join(bucket, "dropped"));
  await writeFile(join(bucket, "dropped", "hello.txt"), "hi");
  await writeFile(join(bucket, "dropped", "meta.json"), `{"note": "from an agent"}`);
  expect((await pushed).params).toMatchObject({ result: [{ id: "dropped", name: "hello.txt", meta: { note: "from an agent" } }] });
});

test("a stream's tail replays, then sends lines as they're appended", async () => {
  const { client } = await start();
  const { dir } = await checkout({ clips: clipsApp });
  result(await client.request("call", { dir, name: "clips.log.append", input: { key: "run-1", data: { text: "a" } } }));

  const subscribed = result(await client.request("subscribe", { dir, subscription: "s1", name: "clips.log.tail", input: { key: "run-1" } }));
  expect(subscribed.stream).toBe(true);
  const replayed = await client.nextPush((push) => push.params.subscription === subscribed.subscription);
  expect(replayed.params).toMatchObject({ items: [{ id: "run-1:0", data: { text: "a" } }] });

  const appended = client.nextPush((push) => push.params.subscription === subscribed.subscription && push !== replayed);
  result(await client.request("call", { dir, name: "clips.log.append", input: { key: "run-1", data: { text: "b" } } }));
  expect((await appended).params).toMatchObject({ items: [{ id: "run-1:1", data: { text: "b" } }] });
});

const tickerApp = `import { definePlugin, method, z } from "@repogo/plugins";
import { join } from "node:path";
export default definePlugin({
  methods: { tick: method(z.object({}), async (_, ctx) => { await Bun.write(join(ctx.root, "ticked"), String(Date.now())); }) },
});
`;

// Its code runs the moment it's imported: the file it leaves says whether it was.
const neighbourApp = `import { definePlugin, method, z } from "@repogo/plugins";
import { join, resolve } from "node:path";
await Bun.write(join(resolve(import.meta.dir, "..", "..", ".."), "neighbour-ran"), "yes");
export default definePlugin({ methods: { hello: method(z.object({}), () => "hi", { kind: "query" }) } });
`;

async function until(check: () => Promise<boolean>, timeout = 4000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    if (await check()) return true;
    await Bun.sleep(50);
  }
  return false;
}

test("schedules run with only their plugins imported; a call opens the rest and keeps them; an empty set stops them", async () => {
  const { client } = await start();
  const { root, dir } = await checkout({ ticker: tickerApp, neighbour: neighbourApp });
  const ticked = { exists: () => Bun.file(join(root, "ticked")).exists(), delete: () => Bun.file(join(root, "ticked")).delete() };
  const neighbourRan = () => Bun.file(join(root, "neighbour-ran")).exists();

  const statuses = result(await client.request("crons", { dir, plugins: ["ticker"], crons: [{ plugin: "ticker", method: "tick", schedule: "* * * * * *" }] }));
  expect(statuses).toMatchObject([{ plugin: "ticker", method: "ticker.tick", error: null }]);
  expect(await until(() => ticked.exists())).toBe(true);
  expect(await neighbourRan()).toBe(false);

  // A card asks for the neighbour: the folder opens in full, and the schedule carries on.
  expect(result(await client.request("call", { dir, name: "neighbour.hello" }))).toBe("hi");
  expect(await neighbourRan()).toBe(true);
  await ticked.delete();
  expect(await until(() => ticked.exists())).toBe(true);

  // An empty set stops it.
  expect(result(await client.request("crons", { dir, plugins: [], crons: [] }))).toEqual([]);
  await Bun.sleep(1200);
  await ticked.delete();
  await Bun.sleep(1500);
  expect(await ticked.exists()).toBe(false);
});

test("a schedule naming what the folder doesn't have says so, and the rest still run", async () => {
  const { client } = await start();
  const { root, dir } = await checkout({ ticker: tickerApp });
  const statuses = result(await client.request("crons", {
    dir, plugins: ["ticker"],
    crons: [{ plugin: "ticker", method: "missing", schedule: "* * * * *" }, { plugin: "ticker", method: "tick", schedule: "* * * * * *" }],
  }));
  expect(statuses[0].error).toContain("isn't one of ticker's methods");
  expect(statuses[1].error).toBeNull();
  expect(await until(() => Bun.file(join(root, "ticked")).exists())).toBe(true);
});

// Imported, it leaves a file; scheduled, it ticks into another.
const markedApp = (name: string) => `import { definePlugin, method, z } from "@repogo/plugins";
import { join, resolve } from "node:path";
const root = resolve(import.meta.dir, "..", "..", "..");
await Bun.write(join(root, "${name}-imported"), "yes");
export default definePlugin({ methods: { tick: method(z.object({}), async () => { await Bun.write(join(root, "${name}-ticked"), String(Date.now())); }) } });
`;

test("a folder opened only for schedules closes when its last one goes", async () => {
  const { client } = await start();
  const { root, dir } = await checkout({ a: markedApp("a") });
  const ticked = () => Bun.file(join(root, "a-ticked")).exists();
  result(await client.request("crons", { dir, plugins: ["a"], crons: [{ plugin: "a", method: "tick", schedule: "* * * * * *" }] }));
  expect(await until(ticked)).toBe(true);
  result(await client.request("crons", { dir, plugins: [], crons: [] }));
  await Bun.sleep(1200);
  await Bun.file(join(root, "a-ticked")).delete();
  await Bun.sleep(1500);
  expect(await ticked()).toBe(false);
});

test("a plugin dropped from a folder's schedules isn't imported again, by the new Worker or a reload", async () => {
  const { client } = await start();
  const { root, dir } = await checkout({ a: markedApp("a"), b: markedApp("b") });
  const file = (name: string) => Bun.file(join(root, name));
  result(await client.request("crons", {
    dir, plugins: ["a", "b"],
    crons: [{ plugin: "a", method: "tick", schedule: "* * * * * *" }, { plugin: "b", method: "tick", schedule: "* * * * * *" }],
  }));
  expect(await until(() => file("b-ticked").exists())).toBe(true);
  await file("b-imported").delete();

  result(await client.request("crons", { dir, plugins: ["a"], crons: [{ plugin: "a", method: "tick", schedule: "* * * * * *" }] }));
  await file("a-ticked").delete();
  expect(await until(() => file("a-ticked").exists())).toBe(true);
  result(await client.request("reload", { dir }));
  await file("a-ticked").delete();
  expect(await until(() => file("a-ticked").exists())).toBe(true);
  expect(await file("b-imported").exists()).toBe(false);
});

test("a plugin's console lines reach the host in one batch, naming the folder and the agent's chat", async () => {
  const { client } = await start();
  const { dir } = await checkout({ chatty: `import { definePlugin, method, z } from "@repogo/plugins";
export default definePlugin({
  methods: {
    hello: method(z.object({}), async () => {
      console.log("one", 1);
      console.error("two");
      return "hi";
    }),
  },
});
` });
  const logged = client.nextPush((push) => push.method === "log");
  expect(result(await client.request("call", { dir, name: "chatty.hello", caller: "agent", chat: "A-1" }))).toBe("hi");
  const push = (await logged) as LogPush;
  expect(push.params.dir).toBe(dir);
  const records = push.params.records;
  expect(records.map((r) => [r.level, r.plugin, r.method, r.caller, r.chatId, r.text])).toEqual([
    ["info", "chatty", "chatty.hello", "agent", "A-1", "one 1"],
    ["error", "chatty", "chatty.hello", "agent", "A-1", "two"],
  ]);
  expect(records[0]!.callId).toBe(records[1]!.callId);
});
