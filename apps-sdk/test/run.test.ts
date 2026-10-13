import { expect, test } from "bun:test";
import { mkdir, mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { run } from "../src/run";

const index = join(import.meta.dir, "../src/index.ts");

async function dirs() {
  const root = await mkdtemp(join(tmpdir(), "repogo-apps-run-"));
  const appsDir = join(root, "apps");
  await mkdir(join(appsDir, "notes"), { recursive: true });
  await writeFile(
    join(appsDir, "notes", "app.ts"),
    `import { defineApp, method, z } from ${JSON.stringify(index)};
export default defineApp({
  description: "Notes",
  collections: { notes: z.object({ text: z.string() }) },
  methods: {
    count: method(z.object({}), async (_, ctx) => (await ctx.db.notes.list()).length, { kind: "query", output: z.number() }),
  },
});`,
  );
  return { appsDir, dataDir: join(root, "data") };
}

test("list prints every app's methods", async () => {
  const { output, ok } = await run(["list"], await dirs());
  expect(ok).toBe(true);
  const names = (output as any).apps[0].methods.map((m: any) => m.name);
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

test("a missing apps folder lists nothing", async () => {
  const root = await mkdtemp(join(tmpdir(), "repogo-apps-run-"));
  const { output } = await run(["list"], { appsDir: join(root, "none"), dataDir: join(root, "data") });
  expect(output).toEqual({ apps: [] });
});
