import { afterEach, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { mkdtemp } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { definePlugin, method, MethodError, stream, toCallError, z } from "../src";
import { Runtime } from "../src/runtime";

const notes = definePlugin({
  description: "Notes",
  namespace: "notes",
  collections: { notes: z.object({ text: z.string(), done: z.boolean().default(false) }) },
  methods: {
    count: method(z.object({}), async (_, ctx) => (await ctx.collections.notes.list()).length, { kind: "query", output: z.number() }),
    archiveAll: method(z.object({}), async (_, ctx) => {
      for (const note of await ctx.collections.notes.list()) await ctx.collections.notes.update(note.id, { done: true });
      return null;
    }),
  },
});

const digest = definePlugin({
  methods: {
    // Reads another plugin through its method: the read is still tracked.
    summary: method(z.object({}), async (_, ctx) => `notes: ${await ctx.call("notes.count")}`, { kind: "query", output: z.string() }),
  },
});

let runtime: Runtime;
let dataDir: string;

async function open() {
  dataDir = await mkdtemp(join(tmpdir(), "repogo-plugins-"));
  runtime = await Runtime.open({ dataDir });
  await runtime.add("notes", notes);
  await runtime.add("digest", digest);
  return runtime;
}

afterEach(() => runtime?.close());

test("every write emits its event", async () => {
  const runtime = await open();
  const seen: string[] = [];
  runtime.events.onAny((event) => void seen.push(event.name));

  const { result } = await runtime.call("notes.notes.create", { text: "hi" });
  const note = result as { id: string };
  await runtime.call("notes.notes.update", { id: note.id, done: true });
  await runtime.call("notes.notes.delete", { id: note.id });

  expect(seen).toEqual([
    "notes.notes.created",
    "notes.notes.create",
    "notes.notes.updated",
    "notes.notes.update",
    "notes.notes.deleted",
    "notes.notes.delete",
  ]);
});

test("a plugin with data gets a folder named by its namespace, one without gets none", async () => {
  await open();
  expect(existsSync(join(dataDir, "notes", "data.db"))).toBe(true);
  expect(existsSync(join(dataDir, "digest"))).toBe(false);
});

test("namespace names the folder and the tag; two plugins naming one namespace share its tables", async () => {
  const runtime = await open();
  const schema = z.object({ text: z.string() });
  await runtime.add("mine", definePlugin({ namespace: "repogo.todos", collections: { items: schema } }));
  await runtime.add("yours", definePlugin({ namespace: "repogo.todos", collections: { items: schema } }));
  expect(existsSync(join(dataDir, "repogo.todos", "data.db"))).toBe(true);

  const written = await runtime.call("mine.items.create", { text: "a" });
  expect(written.invalidates).toEqual(["repogo.todos.items"]);
  const read = await runtime.call("yours.items.list", {}, { expect: "query" });
  expect((read.result as { text: string }[]).map((item) => item.text)).toEqual(["a"]);
  expect(read.reads).toEqual(["repogo.todos.items"]);

  await expect(runtime.add("bad", definePlugin({ namespace: "Repo Go", collections: { items: schema } }))).rejects.toThrow("namespace");
});

test("a plugin with data and no namespace fails the load", async () => {
  const runtime = await open();
  const unnamed = definePlugin({ collections: { items: z.object({ text: z.string() }) } });
  await expect(runtime.add("unnamed", unnamed)).rejects.toThrow("needs a namespace");
});

test("a plugin with only methods needs no namespace", async () => {
  const runtime = await open();
  await runtime.add("plain", definePlugin({ methods: { ping: method(z.object({}), () => "pong", { kind: "query" }) } }));
  expect((await runtime.call("plain.ping")).result).toBe("pong");
});

test("update changes only the fields it is given, defaults included", async () => {
  const runtime = await open();
  const { result } = await runtime.call("notes.notes.create", { text: "a", done: true });
  const { id } = result as { id: string };
  const updated = (await runtime.call("notes.notes.update", { id, text: "b" })).result as { text: string; done: boolean };
  expect(updated).toMatchObject({ text: "b", done: true });
});

test("calls report what they read and wrote, across plugins", async () => {
  const runtime = await open();
  const changed: string[][] = [];
  runtime.onInvalidate((tags) => void changed.push(tags));

  const write = await runtime.call("notes.notes.create", { text: "a" });
  expect(write.invalidates).toEqual(["notes.notes"]);

  const read = await runtime.call("digest.summary");
  expect(read.result).toBe("notes: 1");
  expect(read.reads).toEqual(["notes.notes"]);
  expect(read.invalidates).toEqual([]);

  await runtime.call("notes.archiveAll");
  expect(changed).toEqual([["notes.notes"], ["notes.notes"]]);
});

test("a card's automatic call refuses anything that writes", async () => {
  const runtime = await open();
  expect((await runtime.call("notes.count", {}, { expect: "query" })).result).toBe(0);
  const error = await runtime.call("notes.notes.create", { text: "a" }, { expect: "query" }).catch((e) => e);
  expect(toCallError(error).code).toBe("FORBIDDEN");
  // Unmarked custom methods are mutations.
  expect(runtime.methods.get("notes.archiveAll")?.kind).toBe("mutation");
});

test("errors are structured, with messages per field", async () => {
  const runtime = await open();
  const invalid = await runtime.call("notes.notes.create", { text: 5 }).catch((e) => e);
  expect(toCallError(invalid)).toMatchObject({ code: "VALIDATION", fieldErrors: { "/text": expect.any(String) } });
  // One failing field speaks for the whole error, in the schema's words.
  const one = z.object({ text: z.string().min(1, "Write a note first") }).safeParse({ text: "" });
  expect(toCallError(one.error)).toMatchObject({ message: "Write a note first", fieldErrors: { "/text": "Write a note first" } });
  const two = z.object({ a: z.string(), b: z.string() }).safeParse({});
  expect(toCallError(two.error).message).toBe("Check the highlighted fields.");
  const missing = await runtime.call("notes.nope").catch((e) => e);
  expect(missing).toBeInstanceOf(MethodError);
  expect(toCallError(missing).code).toBe("NOT_FOUND");
});

test("the manifest has each method's kind, input and output", async () => {
  const runtime = await open();
  const plugin = runtime.manifest().plugins.find((a) => a.name === "notes")!;
  expect(plugin.description).toBe("Notes");
  const list = plugin.methods.find((m) => m.name === "notes.notes.list")!;
  expect(list.kind).toBe("query");
  expect(list.output).toMatchObject({ type: "array", items: { type: "object", properties: { id: { type: "string" }, text: { type: "string" } } } });
  expect(plugin.methods.find((m) => m.name === "notes.notes.create")!.kind).toBe("mutation");
});

test("ctx.plugins lists the plugins beside it, and their methods are what ctx.call reaches", async () => {
  await open();
  await runtime.add("catalog", definePlugin({
    methods: {
      list: method(z.object({}), (_, ctx) => ctx.plugins().map((p) => ({ name: p.name, methods: p.methods.map((m) => m.name) })), { kind: "query" }),
      countVia: method(z.object({}), async (_, ctx) => {
        const count = ctx.plugins().flatMap((p) => p.methods).find((m) => m.name === "notes.count")!;
        return ctx.call(count.name);
      }, { kind: "query" }),
    },
  }));
  const { result } = await runtime.call("catalog.list");
  const listed = result as { name: string; methods: string[] }[];
  expect(listed.map((p) => p.name)).toEqual(["notes", "digest", "catalog"]);
  expect(listed.find((p) => p.name === "notes")!.methods).toContain("notes.notes.create");
  expect((await runtime.call("catalog.countVia")).result).toBe(0);
});

test("a method's confirm reaches the manifest, and a collection's delete asks by default", async () => {
  const runtime = await open();
  await runtime.add("risky", definePlugin({
    methods: {
      wipe: method(z.object({}), () => null, { confirm: "Wipe everything?" }),
      tidy: method(z.object({}), () => null),
    },
  }));
  const methods = runtime.manifest().plugins.flatMap((plugin) => plugin.methods);
  const confirm = (name: string) => methods.find((m) => m.name === name)!.confirm;
  expect(confirm("risky.wipe")).toBe("Wipe everything?");
  expect(confirm("risky.tidy")).toBeNull();
  expect(confirm("notes.notes.delete")).toBe("Delete this record?");
  expect(confirm("notes.notes.create")).toBeNull();
});

test("a tap runs a confirm method only once answered; code calling code isn't asked", async () => {
  const runtime = await open();
  await runtime.add("risky", definePlugin({
    methods: { wipe: method(z.object({}), () => "wiped", { confirm: "Wipe everything?" }) },
  }));
  const asked = await runtime.call("risky.wipe", {}, { confirmed: false }).catch((e) => e);
  expect(toCallError(asked)).toEqual({ code: "CONFIRM", message: "Wipe everything?" });
  expect((await runtime.call("risky.wipe", {}, { confirmed: true })).result).toBe("wiped");
  expect((await runtime.call("risky.wipe")).result).toBe("wiped");
});

test("a query can't write, even through what it calls", async () => {
  const runtime = await open();
  const sneaky = definePlugin({
    methods: {
      write: method(z.object({}), async (_, ctx) => ctx.call("notes.notes.create", { text: "x" }), { kind: "query" }),
      direct: method(z.object({}), async () => (runtime as any).methods.get("notes.notes.create").run({ text: "x", done: false }, {}), { kind: "query" }),
    },
  });
  await runtime.add("sneaky", sneaky);
  expect(toCallError(await runtime.call("sneaky.write", {}, { expect: "query" }).catch((e) => e)).code).toBe("FORBIDDEN");
  expect(toCallError(await runtime.call("sneaky.direct", {}, { expect: "query" }).catch((e) => e)).code).toBe("FORBIDDEN");
  expect((await runtime.call("notes.count")).result).toBe(0);
});

test("writes are announced even when the method throws after them, and after it returns", async () => {
  const runtime = await open();
  const changed: string[][] = [];
  runtime.onInvalidate((tags) => void changed.push(tags));
  const plugin = definePlugin({
    methods: {
      fails: method(z.object({}), async (_, ctx) => {
        await ctx.call("notes.notes.create", { text: "a" });
        throw new Error("after the write");
      }),
      later: method(z.object({}), async (_, ctx) => {
        setTimeout(() => void ctx.call("notes.notes.create", { text: "b" }), 0);
        return null;
      }),
    },
  });
  await runtime.add("plugin", plugin);
  await runtime.call("plugin.fails").catch(() => {});
  expect(changed).toEqual([["notes.notes"]]);
  await runtime.call("plugin.later");
  await new Promise((resolve) => setTimeout(resolve, 20));
  expect(changed.length).toBe(2);
});

test("update leaves a wrapped default alone when the field is left out", async () => {
  const runtime = await open();
  await runtime.add(
    "tagged",
    definePlugin({ namespace: "tagged", collections: { items: z.object({ name: z.string(), tag: z.string().default("x").optional() }) } }),
  );
  const { id } = (await runtime.call("tagged.items.create", { name: "a", tag: "kept" })).result as { id: string };
  const updated = (await runtime.call("tagged.items.update", { id, name: "b" })).result as { tag: string };
  expect(updated.tag).toBe("kept");
});

test("a query is read-only however it's called, even inside a writable call", async () => {
  const runtime = await open();
  const plugin = definePlugin({
    methods: {
      sneaky: method(z.object({}), async (_, ctx) => ctx.collections.items.create({ name: "x" }), { kind: "query" }),
      outer: method(z.object({}), async (_, ctx) => ctx.call("plugin.sneaky")),
    },
    namespace: "plugin",
    collections: { items: z.object({ name: z.string() }) },
  });
  await runtime.add("plugin", plugin);
  expect(toCallError(await runtime.call("plugin.sneaky").catch((e) => e)).code).toBe("FORBIDDEN");
  expect(toCallError(await runtime.call("plugin.outer").catch((e) => e)).code).toBe("FORBIDDEN");
  expect(((await runtime.call("plugin.items.list")).result as unknown[]).length).toBe(0);
});

test("a method that throws undefined still fails", async () => {
  const runtime = await open();
  await runtime.add("odd", definePlugin({ methods: { boom: method(z.object({}), async () => { throw undefined; }) } }));
  let failed = false;
  await runtime.call("odd.boom").catch(() => (failed = true));
  expect(failed).toBe(true);
});

test("late writes are announced by the runtime they belong to", async () => {
  const runtime = await open();
  const other = await Runtime.open({ dataDir: await mkdtemp(join(tmpdir(), "repogo-plugins-other-")) });
  const mine: string[][] = [];
  const theirs: string[][] = [];
  runtime.onInvalidate((tags) => void mine.push(tags));
  other.onInvalidate((tags) => void theirs.push(tags));
  await runtime.add("later", definePlugin({
    methods: { go: method(z.object({}), async (_, ctx) => { setTimeout(() => void ctx.call("notes.notes.create", { text: "b" }), 0); return null; }) },
  }));
  await runtime.call("later.go");
  await new Promise((resolve) => setTimeout(resolve, 20));
  expect(mine).toEqual([["notes.notes"]]);
  expect(theirs).toEqual([]);
  other.close();
});

test("concurrent updates to one record both land", async () => {
  const runtime = await open();
  await runtime.add("pair", definePlugin({ namespace: "pair", collections: { items: z.object({ a: z.number(), b: z.number() }) } }));
  const { id } = (await runtime.call("pair.items.create", { a: 0, b: 0 })).result as { id: string };
  await Promise.all([runtime.call("pair.items.update", { id, a: 1 }), runtime.call("pair.items.update", { id, b: 1 })]);
  expect((await runtime.call("pair.items.get", { id })).result).toMatchObject({ a: 1, b: 1 });
});

test("a handler that writes to the collection it reacts to doesn't deadlock", async () => {
  const runtime = await open();
  await runtime.add("echo", definePlugin({
    namespace: "echo",
    collections: { items: z.object({ n: z.number() }) },
    on: { "items.created": async (item, ctx) => { if (item.n < 2) await ctx.collections.items.create({ n: item.n + 1 }); } },
  }));
  await runtime.call("echo.items.create", { n: 0 });
  expect(((await runtime.call("echo.items.list")).result as unknown[]).length).toBe(3);
});

test("a null filter matches stored nulls", async () => {
  const runtime = await open();
  const plugin = definePlugin({
    namespace: "nulls",
    collections: { items: z.object({ n: z.number().nullable() }) },
    methods: { empty: method(z.object({}), async (_, ctx) => (await ctx.collections.items.list({ where: { n: null } })).length, { kind: "query" }) },
  });
  await runtime.add("nulls", plugin);
  await runtime.call("nulls.items.create", { n: null });
  await runtime.call("nulls.items.create", { n: 1 });
  expect((await runtime.call("nulls.empty")).result).toBe(1);
});

const ticker = definePlugin({
  namespace: "ticker",
  collections: { marks: z.object({ at: z.number() }) },
  methods: {
    ticks: stream(z.object({ count: z.number() }), async function* ({ count }, ctx) {
      for (let i = 1; i <= count && !ctx.signal.aborted; i++) {
        yield i;
        await Bun.sleep(1);
      }
    }),
    forever: stream(z.object({}), async function* () {
      for (let i = 1; ; i++) {
        yield i;
        await Bun.sleep(1);
      }
    }),
    writes: stream(z.object({}), async function* (_, ctx) {
      yield await ctx.collections.marks.create({ at: 1 });
    }),
  },
});

test("a stream passes each item on, and its signal aborts it", async () => {
  await open();
  await runtime.add("ticker", ticker);
  const seen: unknown[] = [];
  await runtime.stream("ticker.ticks", { count: 3 }, { signal: new AbortController().signal }, (item) => seen.push(item));
  expect(seen).toEqual([1, 2, 3]);

  const stop = new AbortController();
  const cut: unknown[] = [];
  await runtime.stream("ticker.forever", {}, { signal: stop.signal }, (item) => {
    cut.push(item);
    if (cut.length === 2) stop.abort();
  });
  expect(cut).toEqual([1, 2]);
});

test("a stream refuses bad input before it starts, and may not write", async () => {
  await open();
  await runtime.add("ticker", ticker);
  const signal = new AbortController().signal;
  expect(() => runtime.stream("ticker.ticks", { count: "x" }, { signal }, () => {})).toThrow();
  expect(() => runtime.stream("notes.count", {}, { signal }, () => {})).toThrow("not a stream");
  const failed = await runtime.stream("ticker.writes", {}, { signal }, () => {}).catch(toCallError);
  expect(failed).toMatchObject({ code: "FORBIDDEN" });
});

test("calling a stream returns what it sent, and whether it ended", async () => {
  await open();
  await runtime.add("ticker", ticker);
  expect((await runtime.call("ticker.ticks", { count: 2 })).result).toEqual({ items: [1, 2], done: true });
  const capped = (await runtime.call("ticker.forever", {})).result as { items: unknown[]; done: boolean };
  expect(capped.items).toHaveLength(200);
  expect(capped.done).toBe(false);
  const methods = runtime.manifest().plugins.find((p) => p.name === "ticker")!.methods;
  expect(methods.find((m) => m.name === "ticker.ticks")?.kind).toBe("stream");
});

test("a schedule that could never run says why, and the good ones beside it are set", async () => {
  await open();
  await runtime.add("jobs", definePlugin({
    methods: {
      job: method(z.object({}), () => null),
      needs: method(z.object({ id: z.string() }), () => null),
      asks: method(z.object({}), () => null, { confirm: "Sure?" }),
      tail: stream(z.object({}), async function* () {}),
    },
  }));
  const cases: [Record<string, string>, string][] = [
    [{ plugin: "jobs", method: "missing", schedule: "* * * * *" }, "isn't one of"],
    [{ plugin: "nobody", method: "job", schedule: "* * * * *" }, "isn't loaded here"],
    [{ plugin: "jobs", method: "job", schedule: "* * *" }, "has 3 fields"],
    [{ plugin: "jobs", method: "job", schedule: "* * * * *", timezone: "Mars/Olympus" }, "isn't a time zone"],
    [{ plugin: "jobs", method: "job", schedule: "0 0 31 2 *" }, "never due"],
    [{ plugin: "jobs", method: "needs", schedule: "* * * * *" }, "needs input"],
    [{ plugin: "jobs", method: "asks", schedule: "* * * * *" }, "asks before it runs"],
    [{ plugin: "jobs", method: "tail", schedule: "* * * * *" }, "is a stream"],
  ];
  const statuses = runtime.setCrons([...cases.map(([entry]) => entry as any), { plugin: "jobs", method: "job", schedule: "*/5 * * * * *" }]);
  for (const [index, [, says]] of cases.entries()) expect(statuses[index].error).toContain(says);
  expect(statuses.at(-1)).toMatchObject({ method: "jobs.job", error: null });
  expect(runtime.crons.map((job) => job.method)).toEqual(["jobs.job"]);
});

test("setting schedules again keeps an unchanged one's state, and drops what's left out", async () => {
  await open();
  await runtime.add("jobs", definePlugin({ methods: { a: method(z.object({}), () => null), b: method(z.object({}), () => null) } }));
  runtime.setCrons([{ plugin: "jobs", method: "a", schedule: "* * * * * *" }, { plugin: "jobs", method: "b", schedule: "* * * * * *" }]);
  const [a] = runtime.crons;
  await Promise.all(runtime.tickCrons(a.nextRunAt!));
  expect(a.runs).toBe(1);
  runtime.setCrons([{ plugin: "jobs", method: "a", schedule: "* * * * * *" }]);
  expect(runtime.crons).toEqual([a]);
  expect(runtime.crons[0].runs).toBe(1);
  // A new schedule is a new job.
  runtime.setCrons([{ plugin: "jobs", method: "a", schedule: "*/2 * * * * *" }]);
  expect(runtime.crons[0].runs).toBe(0);
  runtime.setCrons([]);
  expect(runtime.crons).toEqual([]);
});

test("a folder opened for its schedules imports only the plugins they name", async () => {
  dataDir = await mkdtemp(join(tmpdir(), "repogo-plugins-"));
  const dir = await mkdtemp(join(tmpdir(), "repogo-only-"));
  const write = (name: string, source: string) => Bun.write(join(dir, name, "app.ts"), source);
  await write("wanted", `export default { methods: {} };`);
  await write("neighbour", `throw new Error("the neighbour's code ran");`);
  runtime = await Runtime.open({ dataDir, root: dir });
  await runtime.loadDir(dir, ["wanted"]);
  expect([...runtime.plugins.keys()]).toEqual(["wanted"]);
});

test("a cron runs its method when due, once, and the manifest says how it went", async () => {
  await open();
  const runs: string[] = [];
  await runtime.add("clock", definePlugin({
    namespace: "clock",
    collections: { ticks: z.object({ at: z.number() }) },
    methods: {
      tick: method(z.object({}), async (_, ctx) => { runs.push(ctx.root); await ctx.collections.ticks.create({ at: Date.now() }); }),
      fail: method(z.object({}), () => { throw new Error("boom"); }),
    },
  }));
  runtime.setCrons([{ plugin: "clock", method: "tick", schedule: "*/5 * * * * *", timezone: "UTC" }, { plugin: "clock", method: "fail", schedule: "* * * * *" }]);
  const [tick, fail] = runtime.crons;
  expect(runtime.tickCrons(tick.nextRunAt! - 1)).toHaveLength(0);
  const due = tick.nextRunAt!;
  const moment = Math.max(due, fail.nextRunAt!);
  await Promise.all(runtime.tickCrons(moment));
  expect(runs).toHaveLength(1);
  expect(tick.nextRunAt).toBeGreaterThan(moment);
  // The same moment again starts nothing.
  expect(runtime.tickCrons(moment)).toHaveLength(0);
  const [entry] = runtime.manifest().plugins.filter((p) => p.name === "clock");
  expect(entry.crons.map((c) => [c.method, c.runs, c.lastError, c.running])).toEqual([["clock.tick", 1, null, false], ["clock.fail", 1, "boom", false]]);
  expect(entry.crons[0].timezone).toBe("UTC");
});

test("a cron still running isn't started again, and a time slept through is skipped", async () => {
  await open();
  let finish = () => {};
  let started = 0;
  await runtime.add("slow", definePlugin({
    methods: { work: method(z.object({}), () => { started++; return new Promise<void>((resolve) => { finish = resolve; }); }) },
  }));
  runtime.setCrons([{ plugin: "slow", method: "work", schedule: "* * * * * *" }]);
  const [job] = runtime.crons;
  const first = runtime.tickCrons(job.nextRunAt!);
  // The next second comes while it runs: skipped, not saved for later.
  const during = job.nextRunAt!;
  expect(runtime.tickCrons(during)).toHaveLength(0);
  finish();
  await Promise.all(first);
  expect(started).toBe(1);
  expect(job.nextRunAt!).toBeGreaterThan(during);
  expect(runtime.tickCrons(during)).toHaveLength(0);
  // Ten minutes asleep: the missed second is skipped, and the next one is ahead.
  const late = job.nextRunAt! + 10 * 60_000;
  expect(runtime.tickCrons(late)).toHaveLength(0);
  expect(job.nextRunAt!).toBeGreaterThan(late);
  expect(started).toBe(1);
});

test("private data gets no generated methods, and naming one that isn't there fails the load", async () => {
  await open();
  await runtime.add("audit", definePlugin({
    namespace: "audit",
    collections: { entries: z.object({ text: z.string() }), drafts: z.object({ text: z.string() }) },
    kv: { last: z.string() },
    private: ["entries", "last"],
  }));
  const names = [...runtime.methods.keys()].filter((m) => m.startsWith("audit."));
  expect(names.some((m) => m.startsWith("audit.entries.") || m.startsWith("audit.last."))).toBe(false);
  expect(names).toContain("audit.drafts.create");
  await expect(runtime.add("audit2", definePlugin({ namespace: "audit2", collections: { a: z.object({}) }, private: ["b"] })))
    .rejects.toThrow("keeps no collection, kv, stream or bucket by that name");
});

test("a confirm question names what it acts on, and a bad input fails before it's asked", async () => {
  await open();
  await runtime.add("fleet", definePlugin({
    methods: {
      restart: method(z.object({ machine: z.string().regex(/^[a-z0-9]+$/) }), () => "done", { confirm: "Restart {{machine}}? {{missing}}It drops its connections." }),
    },
  }));
  await expect(runtime.call("fleet.restart", { machine: "m42" }, { confirmed: false })).rejects.toThrow("Restart m42? It drops its connections.");
  const bad = await runtime.call("fleet.restart", { machine: "--all" }, { confirmed: false }).catch((error) => toCallError(error));
  expect(bad).toMatchObject({ code: "VALIDATION" });
  expect((await runtime.call("fleet.restart", { machine: "m42" }, { confirmed: true })).result).toBe("done");
  // The manifest keeps the question as written.
  expect(runtime.manifest().plugins.find((p) => p.name === "fleet")!.methods[0].confirm).toBe("Restart {{machine}}? {{missing}}It drops its connections.");
});

test("deleteMany removes what list would return, offset and field order included", async () => {
  await open();
  await runtime.add("bench", definePlugin({
    namespace: "bench",
    collections: { runs: z.object({ job: z.string(), n: z.number() }) },
    methods: {
      fill: method(z.object({}), async (_, ctx) => {
        for (let n = 1; n <= 5; n++) await ctx.collections.runs.create({ job: "a", n });
        await ctx.collections.runs.create({ job: "b", n: 9 });
      }),
      keepNewest: method(z.object({ keep: z.number() }), (input, ctx) =>
        ctx.collections.runs.deleteMany({ where: { job: "a" }, orderBy: "n", order: "desc", offset: input.keep, limit: 1000 })),
      left: method(z.object({}), async (_, ctx) => (await ctx.collections.runs.list({ orderBy: "n", order: "asc" })).map((r) => `${r.job}${r.n}`), { kind: "query" }),
    },
  }));
  await runtime.call("bench.fill");
  expect((await runtime.call("bench.keepNewest", { keep: 2 })).result).toBe(3);
  expect((await runtime.call("bench.left")).result).toEqual(["a4", "a5", "b9"]);
});

test("ctx.caller is who started the call, and code it calls inherits it", async () => {
  await open();
  await runtime.add("who", definePlugin({
    methods: {
      caller: method(z.object({}), (_, ctx) => ctx.caller, { kind: "query" }),
      relay: method(z.object({}), (_, ctx) => ctx.call("caller"), { kind: "query" }),
    },
  }));
  expect((await runtime.call("who.caller")).result).toBe("code");
  expect((await runtime.call("who.caller", {}, { caller: "agent" })).result).toBe("agent");
  expect((await runtime.call("who.relay", {}, { caller: "card" })).result).toBe("card");
});

test("a cron run reports how long it took, and pages showing crons re-run around it", async () => {
  await open();
  await runtime.add("timed", definePlugin({
    methods: {
      work: method(z.object({}), () => new Promise((resolve) => setTimeout(resolve, 20))),
      crons: method(z.object({}), (_, ctx) => ctx.plugins().find((p) => p.name === "timed")!.crons[0].runs, { kind: "query" }),
    },
  }));
  runtime.setCrons([{ plugin: "timed", method: "work", schedule: "* * * * * *" }]);
  const tags: string[] = [];
  runtime.onInvalidate((written) => tags.push(...written));
  const { reads } = await runtime.call("timed.crons");
  const [job] = runtime.crons.filter((c) => c.plugin === "timed");
  await Promise.all(runtime.tickCrons(job.nextRunAt!));
  expect(job.lastDurationMs).toBeGreaterThanOrEqual(15);
  // What read the crons is told twice: as the run starts, and as it ends.
  expect(tags.filter((tag) => reads.includes(tag))).toHaveLength(2);
});

test("a stream with a confirm asks too, and a stream knows who watches it", async () => {
  await open();
  await runtime.add("tails", definePlugin({
    methods: {
      risky: { ...stream(z.object({}), async function* () { yield 1; }), confirm: "Tail it?" },
      who: stream(z.object({}), async function* (_, ctx) { yield ctx.caller; }),
    },
  }));
  await expect(runtime.call("tails.risky", {}, { confirmed: false })).rejects.toThrow("Tail it?");
  const seen: unknown[] = [];
  await runtime.stream("tails.who", {}, { caller: "card", signal: new AbortController().signal }, (item) => seen.push(item));
  expect(seen).toEqual(["card"]);
});

test("one handler that throws doesn't keep a bulk delete's other events from firing", async () => {
  await open();
  const deleted: string[] = [];
  await runtime.add("bulk", definePlugin({
    namespace: "bulk",
    collections: { rows: z.object({ n: z.number() }) },
    methods: {
      fill: method(z.object({}), async (_, ctx) => { for (const n of [1, 2, 3]) await ctx.collections.rows.create({ n }); }),
      wipe: method(z.object({}), (_, ctx) => ctx.collections.rows.deleteMany({ limit: 10 })),
    },
    on: {
      "rows.deleted": (row: { n: number }) => {
        deleted.push(String(row.n));
        if (row.n === 2) throw new Error("handler bug");
      },
    },
  }));
  await runtime.call("bulk.fill");
  expect((await runtime.call("bulk.wipe")).result).toBe(3);
  expect(deleted.sort()).toEqual(["1", "2", "3"]);
});

test("records sort by any field, by id, and through the generated list", async () => {
  await open();
  await runtime.add("people", definePlugin({
    namespace: "people",
    collections: { people: z.object({ "display-name": z.string() }) },
    methods: {
      names: method(z.object({}), async (_, ctx) => (await ctx.collections.people.list({ orderBy: "display-name", order: "asc" })).map((p) => p["display-name"]), { kind: "query" }),
      byID: method(z.object({}), async (_, ctx) => (await ctx.collections.people.list({ orderBy: "id", order: "asc" })).map((p) => p.id), { kind: "query" }),
    },
  }));
  for (const name of ["Cleo", "Ada", "Bea"]) await runtime.call("people.people.create", { "display-name": name });
  expect((await runtime.call("people.names")).result).toEqual(["Ada", "Bea", "Cleo"]);
  const ids = (await runtime.call("people.byID")).result as string[];
  expect(ids).toEqual([...ids].sort());
  const page = (await runtime.call("people.people.list", { orderBy: "display-name", order: "asc", offset: 1, limit: 1 })).result as any[];
  expect(page.map((p) => p["display-name"])).toEqual(["Bea"]);
});
