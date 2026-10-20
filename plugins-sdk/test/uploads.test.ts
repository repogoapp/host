import { afterEach, beforeEach, expect, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { definePlugin, method, toCallError, upload, z } from "../src";
import type { UploadedFile } from "../src";
import { testRuntime, type TestRuntime } from "../src/testing";

let app: TestRuntime | undefined;
let dir: string;

beforeEach(async () => {
  dir = await mkdtemp(join(tmpdir(), "repogo-uploads-"));
});

afterEach(async () => {
  await app?.close();
  await rm(dir, { recursive: true, force: true });
});

const receipts = definePlugin({
  namespace: "test.receipts",
  buckets: { receipts: z.object({ month: z.string().optional() }) },
  methods: {
    ingest: method(z.object({ files: z.array(upload()), month: z.string() }), async ({ files, month }, ctx) => {
      const kept = [];
      for (const file of files) kept.push(await ctx.buckets.receipts.put({ body: Bun.file(file.path), name: file.name, meta: { month } }));
      return kept.map((file) => file.name);
    }),
    forward: method(z.object({ files: z.array(upload()) }), async ({ files }, ctx) => ctx.call("ingest", { files, month: "may" })),
    remove: method(z.object({ files: z.array(upload()) }), async ({ files }) => files.length, { confirm: "Remove these files?" }),
  },
});

async function staged(name: string, text: string): Promise<UploadedFile> {
  const path = join(dir, name);
  await writeFile(path, text);
  return { id: crypto.randomUUID(), name, type: "text/plain", size: text.length, path };
}

test("an upload reaches the method as input.files and is kept with put", async () => {
  app = await testRuntime({ receipts });
  const file = await staged("a.txt", "hello");
  expect(await app.call("receipts.ingest", { month: "june" }, { files: [file] })).toEqual(["a.txt"]);
  const listed = (await app.call("receipts.receipts.list", {})) as { name: string; meta: { month: string } }[];
  expect(listed.map((f) => [f.name, f.meta.month])).toEqual([["a.txt", "june"]]);
});

test("a hand-written file is refused, and so is files alongside the host's", async () => {
  app = await testRuntime({ receipts });
  const file = await staged("a.txt", "hello");
  const forged = toCallError(await app.call("receipts.ingest", { month: "june", files: [file] }).catch((e) => e));
  expect(forged.code).toBe("VALIDATION");
  const doubled = toCallError(await app.call("receipts.ingest", { month: "june", files: [file] }, { files: [file] }).catch((e) => e));
  expect(doubled.code).toBe("VALIDATION");
});

test("a method the upload's method calls may pass the files on", async () => {
  app = await testRuntime({ receipts });
  const file = await staged("b.txt", "hi");
  expect(await app.call("receipts.forward", {}, { files: [file] })).toEqual(["b.txt"]);
});

test("a confirm asks before the files are used, and the confirmed call gets them", async () => {
  app = await testRuntime({ receipts });
  const file = await staged("c.txt", "x");
  const asked = toCallError(await app.call("receipts.remove", {}, { files: [file], confirmed: false }).catch((e) => e));
  expect(asked.code).toBe("CONFIRM");
  expect(await app.call("receipts.remove", {}, { files: [file], confirmed: true })).toBe(1);
});
