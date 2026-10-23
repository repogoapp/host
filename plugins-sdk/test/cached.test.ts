import { afterEach, expect, setSystemTime, test } from "bun:test";
import { mkdtemp } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { cached, definePlugin, MethodError, z } from "../src";
import { AnswerCache, cacheKey, type CacheOptions } from "../src/cache";
import { Database } from "../src/database";
import { Runtime } from "../src/runtime";

const options: CacheOptions = { fresh: 60_000, shared: false, timeout: 60_000 };
let database: Database | undefined;
let runtime: Runtime | undefined;

afterEach(() => {
  setSystemTime();
  database?.close();
  runtime?.close();
  database = runtime = undefined;
});

async function open() {
  const dir = await mkdtemp(join(tmpdir(), "repogo-cache-"));
  database = await Database.open(join(dir, "data.db"));
  const announced: string[][] = [];
  const cache = new AnswerCache(database, "test", (tags) => announced.push(tags));
  await cache.setup();
  return { cache, announced };
}

test("the first read waits for the fetch, and later ones answer from what it saved", async () => {
  const { cache, announced } = await open();
  let fetches = 0;
  const fetch = () => ({ projects: ["a"], fetch: ++fetches });

  expect(await cache.read("k", options, fetch)).toEqual({ projects: ["a"], fetch: 1 });
  expect(await cache.read("k", options, fetch)).toEqual({ projects: ["a"], fetch: 1 });
  expect(fetches).toBe(1);
  expect(announced).toHaveLength(1);
});

test("a stale answer comes back at once and a changed one is announced", async () => {
  const { cache, announced } = await open();
  let answer = "old";
  await cache.read("k", options, () => answer);
  setSystemTime(Date.now() + options.fresh);

  answer = "new";
  expect(await cache.read("k", options, () => answer)).toBe("old");
  await cache.idle();
  expect(announced).toHaveLength(2);
  expect(await cache.read("k", options, () => "never")).toBe("new");
});

test("checking an unchanged answer announces nothing and makes it fresh again", async () => {
  const { cache, announced } = await open();
  let fetches = 0;
  const fetch = () => (fetches++, "same");
  await cache.read("k", options, fetch);
  setSystemTime(Date.now() + options.fresh);

  await cache.read("k", options, fetch);
  await cache.idle();
  await cache.read("k", options, fetch);
  await cache.idle();
  expect(fetches).toBe(2);
  expect(announced).toHaveLength(1);
});

test("a failed check keeps the saved answer and waits before trying again", async () => {
  const { cache } = await open();
  await cache.read("k", options, () => "saved");
  setSystemTime(Date.now() + options.fresh);

  let fetches = 0;
  const failing = () => { fetches++; throw new MethodError("INTERNAL", "offline"); };
  expect(await cache.read("k", options, failing)).toBe("saved");
  await cache.idle();
  expect(await cache.read("k", options, failing)).toBe("saved");
  await cache.idle();
  expect(fetches).toBe(1);
});

test("with nothing saved, a failed fetch is the caller's error", async () => {
  const { cache } = await open();
  await expect(cache.read("k", options, () => { throw new MethodError("INTERNAL", "offline"); })).rejects.toThrow("offline");
});

test("reads waiting on the same key share one fetch", async () => {
  const { cache } = await open();
  let fetches = 0;
  let finish!: (value: string) => void;
  const fetch = () => (fetches++, new Promise<string>((resolve) => (finish = resolve)));

  const first = cache.read("k", options, fetch);
  const second = cache.read("k", options, fetch);
  while (!finish) await Promise.resolve();
  finish("one");
  expect(await Promise.all([first, second])).toEqual(["one", "one"]);
  expect(fetches).toBe(1);
});

test("keys sort the input's fields and keep projects apart", () => {
  expect(cacheKey("p.m", "/a", { b: 1, a: { d: 2, c: 3 } })).toBe(cacheKey("p.m", "/a", { a: { c: 3, d: 2 }, b: 1 }));
  expect(cacheKey("p.m", "/a", {})).not.toBe(cacheKey("p.m", "/b", {}));
});

test("a cached method answers from what it saved and depends only on its own answer", async () => {
  const dataDir = await mkdtemp(join(tmpdir(), "repogo-plugins-"));
  runtime = await Runtime.open({ dataDir });
  let fetches = 0;
  await runtime.add("slow", definePlugin({
    namespace: "slow",
    kv: { notes: z.string() },
    methods: {
      projects: cached(z.object({ limit: z.number().default(10) }), (input) => ({ fetch: ++fetches, limit: input.limit }), { fresh: "1m" }),
      writes: cached(z.object({}), async (_, ctx) => ctx.kv.notes.set("k", "v"), { fresh: "1m" }),
    },
  }));

  const first = await runtime.call("slow.projects", {}, { expect: "query" });
  const again = await runtime.call("slow.projects", { limit: 10 }, { expect: "query" });
  expect(again.result).toEqual({ fetch: 1, limit: 10 });
  expect(again.reads).toEqual(first.reads);
  expect(again.reads).toHaveLength(1);
  expect(again.reads[0]).toStartWith("slow.cached.");

  await expect(runtime.call("slow.writes", {}, { expect: "query" })).rejects.toThrow("can't change data");
});
