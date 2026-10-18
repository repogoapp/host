import { expect, test } from "bun:test";
import { definePlugin, method, MethodError, z } from "../src";
import { testRuntime } from "../src/testing";

const notes = definePlugin({
  namespace: "notes",
  collections: { notes: z.object({ text: z.string() }) },
  methods: {
    add: method(z.object({ text: z.string() }), (input, ctx) => ctx.collections.notes.create(input)),
    count: method(z.object({}), async (_, ctx) => (await ctx.collections.notes.list()).length, { kind: "query" }),
    clear: method(z.object({}), (_, ctx) => ctx.collections.notes.deleteMany({ limit: 1000 }), { confirm: "Delete every note?" }),
    ping: method(z.object({}), (_, ctx) => { ctx.notify({ title: "Ping", body: ctx.caller }); }),
  },
});

test("a test runtime runs plugins with real data, confirms, crons and notices, then cleans up", async () => {
  const app = await testRuntime({ notes });
  await app.call("notes.add", { text: "a" });
  await app.call("notes.add", { text: "b" });
  expect(await app.call("notes.count")).toBe(2);
  await expect(app.call("notes.clear", {}, { confirmed: false })).rejects.toBeInstanceOf(MethodError);
  expect(await app.call("notes.clear", {}, { confirmed: true })).toBe(2);
  expect(app.setCrons([{ plugin: "notes", method: "ping", schedule: "* * * * *" }])[0].error).toBeNull();
  await app.tickCrons(app.manifest().plugins[0].crons[0].nextRunAt!);
  expect(app.notices.map((n) => n.body)).toEqual(["cron"]);
  await app.close();
});

test("ctx.root is an empty folder of its own unless the test names one", async () => {
  const where = definePlugin({ methods: { root: method(z.object({}), (_, ctx) => ctx.root, { kind: "query" }) } });
  const app = await testRuntime({ where });
  const root = (await app.call("where.root")) as string;
  expect(root).not.toBe(process.cwd());
  expect(await Array.fromAsync(new Bun.Glob("*").scan({ cwd: root }))).toEqual([]);
  await app.close();
  const fixture = await testRuntime({ where }, { root: "/tmp" });
  expect(await fixture.call("where.root")).toBe("/tmp");
  await fixture.close();
});

test("a plugin that can't load fails the test runtime with its reason", async () => {
  await expect(testRuntime({ broken: definePlugin({ collections: { a: z.object({}) } }) })).rejects.toThrow("needs a namespace");
});
