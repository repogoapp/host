import { afterEach, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { appendFile, mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { definePlugin, method, toCallError, z } from "../src";
import type { BucketFile, Context, KvEntry, PluginDefinition, StreamEntry, StreamKey } from "../src";
import { Runtime } from "../src/runtime";

let runtime: Runtime;
let dataDir: string;

async function open(plugins: Record<string, ReturnType<typeof definePlugin>>) {
  dataDir = await mkdtemp(join(tmpdir(), "repogo-storage-"));
  runtime = await Runtime.open({ dataDir });
  for (const [name, plugin] of Object.entries(plugins)) await runtime.add(name, plugin);
  return runtime;
}

afterEach(() => runtime?.close());

const flows = withContext({
  namespace: "repo.flows",
  kv: { state: z.object({ step: z.string() }) },
  streams: { runs: z.object({ text: z.string() }) },
  buckets: { photos: z.object({ caption: z.string().optional(), tags: z.array(z.string()).optional() }) },
});

// Kv

test("kv sets, reads, lists by prefix and deletes, with tags and events", async () => {
  const runtime = await open({ flows });
  const seen: string[] = [];
  runtime.events.onAny((event) => void seen.push(event.name));

  const set = await runtime.call("flows.state.set", { key: "run:1", value: { step: "build" } });
  expect(set.invalidates).toEqual(["repo.flows.kv.state"]);
  await runtime.call("flows.state.set", { key: "run:1", value: { step: "test" } });
  await runtime.call("flows.state.set", { key: "other", value: { step: "x" } });

  const got = await runtime.call("flows.state.get", { key: "run:1" }, { expect: "query" });
  expect(got.reads).toEqual(["repo.flows.kv.state"]);
  expect((got.result as KvEntry<{ step: string }>).value).toEqual({ step: "test" });

  const listed = (await runtime.call("flows.state.list", { prefix: "run:" })).result as KvEntry<unknown>[];
  expect(listed.map((entry) => entry.key)).toEqual(["run:1"]);

  await runtime.call("flows.state.delete", { key: "run:1" });
  expect((await runtime.call("flows.state.get", { key: "run:1" })).result).toBeNull();
  expect(seen.filter((name) => /^flows\.state\.(created|updated|deleted)$/.test(name))).toEqual([
    "flows.state.created",
    "flows.state.updated",
    "flows.state.created",
    "flows.state.deleted",
  ]);
});

test("a kv key past its ttl reads as missing, and the sweep deletes it and tells cards", async () => {
  const runtime = await open({ flows });
  const kv = (await contextOf(runtime)).kv.state;
  await kv.set("lock", { step: "a" }, { ttl: 1 });
  await Bun.sleep(5);
  expect(await kv.get("lock")).toBeNull();
  expect(await kv.list()).toEqual([]);

  const told: string[][] = [];
  runtime.onInvalidate((tags) => void told.push(tags));
  await runtime.tickStorage();
  expect(told).toEqual([["repo.flows.kv.state"]]);
});

test("setting a kv key again without a ttl keeps it", async () => {
  const runtime = await open({ flows });
  const kv = (await contextOf(runtime)).kv.state;
  await kv.set("k", { step: "a" }, { ttl: 1 });
  await kv.set("k", { step: "b" });
  await Bun.sleep(5);
  await runtime.tickStorage();
  expect(await kv.get("k")).toEqual({ step: "b" });
});

test("a kv value is checked against its schema", async () => {
  const runtime = await open({ flows });
  const error = await runtime.call("flows.state.set", { key: "k", value: { step: 1 } }).catch((e) => e);
  expect(toCallError(error).code).toBe("VALIDATION");
});

// Streams

test("a stream appends JSONL lines with offsets and reads them back", async () => {
  const runtime = await open({ flows });
  for (const text of ["a", "b", "c"]) await runtime.call("flows.runs.append", { key: "run-1", data: { text } });

  const file = join(dataDir, "repo.flows", "streams", "runs", "run-1", "000000000000.jsonl");
  const lines = (await readFile(file, "utf8")).trim().split("\n").map((line) => JSON.parse(line));
  expect(lines.map((line) => [line.offset, line.data.text])).toEqual([[0, "a"], [1, "b"], [2, "c"]]);

  const read = (await runtime.call("flows.runs.read", { key: "run-1", from: 1 })).result as StreamEntry<{ text: string }>[];
  expect(read.map((line) => [line.id, line.data.text])).toEqual([["run-1:1", "b"], ["run-1:2", "c"]]);

  const keys = (await runtime.call("flows.runs.keys", {})).result as StreamKey[];
  expect(keys).toMatchObject([{ key: "run-1", lines: 3, endedAt: null }]);
});

test("a line written past the committed length is cut before the next append", async () => {
  const runtime = await open({ flows });
  const runs = (await contextOf(runtime)).streams.runs;
  await runs.append("r", { text: "a" });
  // A crash between writing the line and committing its row leaves this behind.
  const file = join(dataDir, "repo.flows", "streams", "runs", "r", "000000000000.jsonl");
  await appendFile(file, `{"offset":1,"at":"x","data":{"text":"lost"}}\n`);
  expect((await runs.read("r")).map((line) => line.data.text)).toEqual(["a"]);

  await runs.append("r", { text: "b" });
  expect((await runs.read("r")).map((line) => [line.offset, line.data.text])).toEqual([[0, "a"], [1, "b"]]);
});

test("follow replays, then yields new lines, and ends with the key", async () => {
  const runtime = await open({ flows });
  const runs = (await contextOf(runtime)).streams.runs;
  await runs.append("r", { text: "a" });
  const seen: string[] = [];
  const following = (async () => {
    for await (const line of runs.follow("r", { signal: new AbortController().signal })) seen.push(line.data.text);
  })();
  await runs.append("r", { text: "b" });
  await runs.end("r");
  await following;
  expect(seen).toEqual(["a", "b"]);

  const error = await runs.append("r", { text: "c" }).catch((e) => e);
  expect(toCallError(error).message).toContain("has ended");
});

test("tail is a stream method cards can watch", async () => {
  const runtime = await open({ flows });
  expect(runtime.methods.get("flows.runs.tail")?.kind).toBe("stream");
  await runtime.call("flows.runs.append", { key: "r", data: { text: "a" } });
  await runtime.call("flows.runs.end", { key: "r" });
  const items: unknown[] = [];
  await runtime.stream("flows.runs.tail", { key: "r" }, { signal: new AbortController().signal }, (item) => void items.push(item));
  expect(items).toMatchObject([{ id: "r:0", data: { text: "a" } }]);
});

test("retention drops a key untouched past its age, and old segments past its bytes", async () => {
  const runtime = await open({
    logs: withContext({ namespace: "logs", streams: { old: z.object({ text: z.string() }), big: { item: z.object({ text: z.string() }), retain: { bytes: 1_500_000 } } } }),
  });
  const { old, big } = (await contextOf(runtime, "logs")).streams;
  await old.append("k", { text: "a" });
  // Eight days on, the key is past the default seven.
  await runtime.tickStorage(Date.now() + 8 * 86_400_000);
  expect(await old.keys()).toEqual([]);
  expect(existsSync(join(dataDir, "logs", "streams", "old", "k"))).toBe(false);

  // About 100 KB a line: segments roll every 1 MB, and the oldest go past 1.5 MB.
  const text = "x".repeat(100_000);
  for (let i = 0; i < 30; i++) await big.append("k", { text });
  // Retention ran at day 8 above; it runs again a minute or more later.
  await runtime.tickStorage(Date.now() + 9 * 86_400_000);
  const [key] = await big.keys();
  expect(key!.bytes).toBeLessThanOrEqual(1_500_000 + 1_000_000);
  const kept = await big.read("k", { limit: 500 });
  expect(kept[0]!.offset).toBeGreaterThan(0);
  expect(kept.at(-1)!.offset).toBe(29);
});

// Buckets

test("put writes a folder per file with meta.json, and get, list and search find it", async () => {
  const runtime = await open({ flows });
  const put = await runtime.call("flows.photos.put", { name: "beach day.txt", folder: "2026", text: "sand", meta: { caption: "Sunset trip" } });
  const file = put.result as BucketFile<{ caption: string }>;
  expect(put.invalidates).toEqual(["repo.flows.buckets.photos"]);
  expect(file).toMatchObject({ name: "beach day.txt", folder: "2026", size: 4, status: "ok", meta: { caption: "Sunset trip" }, revision: 1 });
  expect(await readFile(file.path, "utf8")).toBe("sand");
  expect(JSON.parse(await readFile(join(dataDir, "repo.flows", "buckets", "photos", file.id, "meta.json"), "utf8"))).toEqual({
    caption: "Sunset trip",
    folder: "2026",
  });

  expect(((await runtime.call("flows.photos.get", { id: file.id })).result as BucketFile<unknown>).id).toBe(file.id);
  expect(((await runtime.call("flows.photos.list", { folder: "2026" })).result as unknown[]).length).toBe(1);
  expect(((await runtime.call("flows.photos.list", { folder: "2025" })).result as unknown[]).length).toBe(0);
  const found = (await runtime.call("flows.photos.search", { query: "suns" }, { expect: "query" })).result as BucketFile<unknown>[];
  expect(found.map((f) => f.id)).toEqual([file.id]);
  expect(((await runtime.call("flows.photos.search", { query: "beach" })).result as unknown[]).length).toBe(1);
  expect(((await runtime.call("flows.photos.search", { query: "winter" })).result as unknown[]).length).toBe(0);
});

test("a file an agent drops in is found by a scan, with empty, malformed and invalid meta told apart", async () => {
  const runtime = await open({ flows });
  const bucket = (await contextOf(runtime)).buckets.photos;
  const created: string[] = [];
  runtime.events.on("flows.photos.created", (event) => void created.push((event.payload as BucketFile<unknown>).name));

  const add = async (id: string, name: string, meta?: string) => {
    const folder = join(bucket.path, id);
    await mkdir(folder, { recursive: true });
    await writeFile(join(folder, name), "x");
    if (meta !== undefined) await writeFile(join(folder, "meta.json"), meta);
  };
  await add("a", "plain.txt");
  await add("b", "broken.txt", "{nope");
  await add("c", "wrong.txt", `{"caption": 5}`);
  await add("d", "good.txt", `{"caption": "hi", "notes": "kept"}`);
  await bucket.scan();

  const files = Object.fromEntries((await bucket.list()).map((file) => [file.id, file]));
  expect(files.a).toMatchObject({ status: "empty", problem: null, meta: {} });
  expect(files.b).toMatchObject({ status: "malformed", problem: expect.stringContaining("meta.json") });
  expect(files.c).toMatchObject({ status: "invalid", problem: expect.any(String) });
  expect(files.d).toMatchObject({ status: "ok", meta: { caption: "hi", notes: "kept" } });
  expect(created.sort()).toEqual(["broken.txt", "good.txt", "plain.txt", "wrong.txt"]);

  // Another scan with nothing changed emits nothing.
  await bucket.scan();
  expect(created.length).toBe(4);
});

test("a scan tells a changed file from changed meta, and a removed folder is deleted", async () => {
  const runtime = await open({ flows });
  const bucket = (await contextOf(runtime)).buckets.photos;
  const seen: string[] = [];
  runtime.events.onAny((event) => void seen.push(event.name));
  const file = await bucket.put({ name: "a.txt", body: "one" });
  seen.length = 0;

  await writeFile(file.path, "two!");
  await bucket.scan();
  await writeFile(join(bucket.path, file.id, "meta.json"), `{"caption": "now"}`);
  await bucket.scan();
  expect(seen).toEqual(["flows.photos.changed", "flows.photos.metaChanged"]);
  expect(await bucket.get(file.id)).toMatchObject({ size: 4, meta: { caption: "now" }, revision: 3 });

  await rm(join(bucket.path, file.id), { recursive: true });
  await bucket.scan();
  expect(await bucket.get(file.id)).toBeNull();
  expect(seen.at(-1)).toBe("flows.photos.deleted");
});

test("update merges meta, checks the schema and the revision; move renames in place", async () => {
  const runtime = await open({ flows });
  const bucket = (await contextOf(runtime)).buckets.photos;
  const file = await bucket.put({ name: "a.txt", body: "x", folder: "inbox", meta: { caption: "one" } });

  const updated = await bucket.update(file.id, { tags: ["trip"] }, { revision: file.revision });
  expect(updated).toMatchObject({ folder: "inbox", meta: { caption: "one", tags: ["trip"] }, revision: 2 });
  const cleared = await bucket.update(file.id, { caption: null });
  expect(cleared.meta).toEqual({ tags: ["trip"] });

  const stale = await bucket.update(file.id, { caption: "late" }, { revision: file.revision }).catch((e) => e);
  expect(toCallError(stale).message).toContain("changed since it was read");
  const invalid = await bucket.update(file.id, { caption: 5 as never }).catch((e) => e);
  expect(toCallError(invalid).code).toBe("VALIDATION");

  const moved = await bucket.move(file.id, { name: "b.txt", folder: "done" });
  expect(moved).toMatchObject({ id: file.id, name: "b.txt", folder: "done", meta: { tags: ["trip"] } });
  expect(await readdir(join(bucket.path, file.id))).toEqual(expect.arrayContaining(["b.txt", "meta.json"]));
});

test("delete removes the folder and asks first from a card", async () => {
  const runtime = await open({ flows });
  const bucket = (await contextOf(runtime)).buckets.photos;
  const file = await bucket.put({ name: "a.txt", body: "x" });
  const asked = await runtime.call("flows.photos.delete", { id: file.id }, { confirmed: false }).catch((e) => e);
  expect(toCallError(asked).code).toBe("CONFIRM");
  await runtime.call("flows.photos.delete", { id: file.id });
  expect(existsSync(join(bucket.path, file.id))).toBe(false);
  expect(await bucket.get(file.id)).toBeNull();
});

test("a file dropped in without a call reaches cards, by the watcher or the next scan", async () => {
  const runtime = await open({ flows });
  const bucket = (await contextOf(runtime)).buckets.photos;
  // A scan may land between the folder and its file; the next one finds the file.
  const found = new Promise<void>((resolve) => runtime.onInvalidate(async (tags) => {
    if (tags.includes("repo.flows.buckets.photos") && (await bucket.get("dropped"))?.name === "note.txt") resolve();
  }));
  await mkdir(join(bucket.path, "dropped"));
  await writeFile(join(bucket.path, "dropped", "note.txt"), "hi");
  // The watcher is a hint and can miss events; the minute's scan is the backstop.
  const backstop = setTimeout(() => void runtime.tickStorage(Date.now() + 61_000), 2_000);
  await found;
  clearTimeout(backstop);
  expect(await bucket.get("dropped")).toMatchObject({ name: "note.txt", status: "empty" });
});

test("a file name can't escape its folder or be meta.json", async () => {
  const runtime = await open({ flows });
  const bucket = (await contextOf(runtime)).buckets.photos;
  for (const name of ["../x", "a/b", ".hidden", "meta.json"]) {
    const error = await bucket.put({ name, body: "x" }).catch((e) => e);
    expect(toCallError(error).code).toBe("VALIDATION");
  }
  const error = await bucket.get("../../etc");
  expect(error).toBeNull();
});

// Declaring

test("plugins naming one bucket share it; a different schema fails the load", async () => {
  const runtime = await open({ flows, more: definePlugin({ namespace: "repo.flows", buckets: { photos: flows.buckets!.photos } }) });
  await runtime.call("flows.photos.put", { name: "a.txt", text: "x" });
  expect(((await runtime.call("more.photos.list", {})).result as unknown[]).length).toBe(1);

  const different = definePlugin({ namespace: "repo.flows", buckets: { photos: z.object({ other: z.string() }) } });
  await expect(runtime.add("third", different)).rejects.toThrow("different schema");
});

test("a method and generated data of the same name fail the load", async () => {
  const runtime = await open({});
  const clash = definePlugin({
    namespace: "clash",
    kv: { state: z.string() },
    methods: { "state.get": { kind: "query", input: z.object({}), run: () => null } },
  });
  await expect(runtime.add("clash", clash)).rejects.toThrow("defined twice");
});

test("the manifest lists where buckets and streams live", async () => {
  const runtime = await open({ flows });
  const plugin = runtime.manifest().plugins.find((p) => p.name === "flows")!;
  expect(plugin.storage).toEqual({
    buckets: [{ name: "photos", path: join(dataDir, "repo.flows", "buckets", "photos") }],
    kv: [{ name: "state" }],
    streams: [{ name: "runs", path: join(dataDir, "repo.flows", "streams", "runs") }],
  });
});

/** The plugin's own `ctx`, as its methods get it, to use the SDK directly. */
async function contextOf(runtime: Runtime, plugin = "flows"): Promise<Context> {
  return (await runtime.call(`${plugin}.ctx`)).result as Context;
}

/** A plugin with a `ctx` method returning its context. */
function withContext<T extends PluginDefinition>(plugin: T): T {
  return { ...plugin, methods: { ...plugin.methods, ctx: method(z.object({}), (_, ctx) => ctx, { kind: "query" }) } };
}
