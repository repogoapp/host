import { afterEach, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { mkdtemp } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { definePlugin, method, MethodError, stream, toCallError, z } from "../src";
import { Runtime } from "../src/runtime";

const notes = definePlugin({
  description: "Notes",
  collections: { notes: z.object({ text: z.string(), done: z.boolean().default(false) }) },
  methods: {
    count: method(z.object({}), async (_, ctx) => (await ctx.db.notes.list()).length, { kind: "query", output: z.number() }),
    archiveAll: method(z.object({}), async (_, ctx) => {
      for (const note of await ctx.db.notes.list()) await ctx.db.notes.update(note.id, { done: true });
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

test("a plugin with collections gets a file named by its db, one without gets none", async () => {
  await open();
  expect(existsSync(join(dataDir, "notes.db"))).toBe(true);
  expect(existsSync(join(dataDir, "digest.db"))).toBe(false);
});

test("db names the file and the tag; two plugins naming one db share its tables", async () => {
  const runtime = await open();
  const schema = z.object({ text: z.string() });
  await runtime.add("mine", definePlugin({ db: "repogo.todos", collections: { items: schema } }));
  await runtime.add("yours", definePlugin({ db: "repogo.todos", collections: { items: schema } }));
  expect(existsSync(join(dataDir, "repogo.todos.db"))).toBe(true);

  const written = await runtime.call("mine.items.create", { text: "a" });
  expect(written.invalidates).toEqual(["repogo.todos.items"]);
  const read = await runtime.call("yours.items.list", {}, { expect: "query" });
  expect((read.result as { text: string }[]).map((item) => item.text)).toEqual(["a"]);
  expect(read.reads).toEqual(["repogo.todos.items"]);

  await expect(runtime.add("bad", definePlugin({ db: "Repo Go", collections: { items: schema } }))).rejects.toThrow("db");
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
    definePlugin({ collections: { items: z.object({ name: z.string(), tag: z.string().default("x").optional() }) } }),
  );
  const { id } = (await runtime.call("tagged.items.create", { name: "a", tag: "kept" })).result as { id: string };
  const updated = (await runtime.call("tagged.items.update", { id, name: "b" })).result as { tag: string };
  expect(updated.tag).toBe("kept");
});

test("a query is read-only however it's called, even inside a writable call", async () => {
  const runtime = await open();
  const plugin = definePlugin({
    methods: {
      sneaky: method(z.object({}), async (_, ctx) => ctx.db.items.create({ name: "x" }), { kind: "query" }),
      outer: method(z.object({}), async (_, ctx) => ctx.call("plugin.sneaky")),
    },
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
  await runtime.add("pair", definePlugin({ collections: { items: z.object({ a: z.number(), b: z.number() }) } }));
  const { id } = (await runtime.call("pair.items.create", { a: 0, b: 0 })).result as { id: string };
  await Promise.all([runtime.call("pair.items.update", { id, a: 1 }), runtime.call("pair.items.update", { id, b: 1 })]);
  expect((await runtime.call("pair.items.get", { id })).result).toMatchObject({ a: 1, b: 1 });
});

test("a handler that writes to the collection it reacts to doesn't deadlock", async () => {
  const runtime = await open();
  await runtime.add("echo", definePlugin({
    collections: { items: z.object({ n: z.number() }) },
    on: { "items.created": async (item, ctx) => { if (item.n < 2) await ctx.db.items.create({ n: item.n + 1 }); } },
  }));
  await runtime.call("echo.items.create", { n: 0 });
  expect(((await runtime.call("echo.items.list")).result as unknown[]).length).toBe(3);
});

test("a null filter matches stored nulls", async () => {
  const runtime = await open();
  const plugin = definePlugin({
    collections: { items: z.object({ n: z.number().nullable() }) },
    methods: { empty: method(z.object({}), async (_, ctx) => (await ctx.db.items.list({ where: { n: null } })).length, { kind: "query" }) },
  });
  await runtime.add("nulls", plugin);
  await runtime.call("nulls.items.create", { n: null });
  await runtime.call("nulls.items.create", { n: 1 });
  expect((await runtime.call("nulls.empty")).result).toBe(1);
});

const ticker = definePlugin({
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
      yield await ctx.db.marks.create({ at: 1 });
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
