import { expect, test } from "bun:test";
import { mkdir, mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { run } from "../src/run";

const index = join(import.meta.dir, "../src/index.ts");

async function dirs() {
  const root = await mkdtemp(join(tmpdir(), "repogo-plugins-run-"));
  const pluginsDir = join(root, "plugins");
  await mkdir(join(pluginsDir, "notes"), { recursive: true });
  await writeFile(
    join(pluginsDir, "notes", "app.ts"),
    `import { definePlugin, method, z } from ${JSON.stringify(index)};
export default definePlugin({
  description: "Notes",
  namespace: "notes",
  collections: { notes: z.object({ text: z.string() }) },
  methods: {
    count: method(z.object({}), async (_, ctx) => (await ctx.collections.notes.list()).length, { kind: "query", output: z.number() }),
  },
});`,
  );
  return { pluginsDir, dataDir: join(root, "data") };
}

test("list prints every plugin's methods", async () => {
  const { output, ok } = await run(["list"], await dirs());
  expect(ok).toBe(true);
  const names = (output as any).plugins[0].methods.map((m: any) => m.name);
  expect(names).toContain("notes.count");
  expect(names).toContain("notes.notes.create");
});

test("a call reports its result and what it wrote, and the data outlives the process", async () => {
  const d = await dirs();
  const created = await run(["call", "notes.notes.create", JSON.stringify({ text: "hi" })], d);
  expect(created.ok).toBe(true);
  expect((created.output as any).invalidates).toEqual(["notes.notes"]);
  const counted = await run(["call", "notes.count"], d);
  expect((counted.output as any).result).toBe(1);
});

test("--query refuses a mutation", async () => {
  const { output, ok } = await run(["call", "notes.notes.create", JSON.stringify({ text: "hi" }), "--query"], await dirs());
  expect(ok).toBe(false);
  expect((output as any).error.code).toBe("FORBIDDEN");
});

test("a bad input comes back as field errors", async () => {
  const { output, ok } = await run(["call", "notes.notes.create", "{}"], await dirs());
  expect(ok).toBe(false);
  expect((output as any).error.fieldErrors["/text"]).toBeDefined();
});

test("a missing plugins folder lists nothing", async () => {
  const root = await mkdtemp(join(tmpdir(), "repogo-plugins-run-"));
  const { output } = await run(["list"], { pluginsDir: join(root, "none"), dataDir: join(root, "data") });
  expect(output).toEqual({ plugins: [] });
});
