import { expect, test } from "bun:test";
import { mkdtemp } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { definePlugin, method, MethodError, z } from "../src";
import { Runtime } from "../src/runtime";
import { testRuntime, type TestMCPCall } from "../src/testing";

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

const shop = definePlugin({
  methods: {
    customers: method(z.object({}), (_, ctx) => ctx.mcp("stripe", "list_customers", { limit: 2 }), { kind: "query" }),
    create: method(z.object({ email: z.string() }), (input, ctx) => ctx.mcp("stripe", "create_customer", input)),
    remove: method(z.object({ id: z.string() }), (input, ctx) => ctx.mcp("stripe", "delete_customer", input), { confirm: "Delete {{id}}?" }),
    // Called by remove's neighbour through ctx.call: it runs under the confirm the user answered.
    purge: method(z.object({}), (_, ctx) => ctx.call("remove", { id: "cus_1" }), { confirm: "Delete everything?" }),
    sneak: method(z.object({}), (_, ctx) => ctx.mcp("stripe", "create_customer", {}), { kind: "query" }),
  },
});

test("ctx.mcp asks for the calling project's tool, read-only in a query and confirmed only under a confirm", async () => {
  const asked: TestMCPCall[] = [];
  const app = await testRuntime({ shop }, {
    root: "/tmp",
    mcp: (call) => {
      asked.push(call);
      if (call.read && call.tool !== "list_customers") throw new Error("NOT_READ_ONLY");
      return call.tool === "list_customers" ? [{ id: "cus_1" }] : { id: "cus_2" };
    },
  });
  expect(await app.call("shop.customers")).toEqual([{ id: "cus_1" }]);
  expect(asked[0]).toEqual({ project: "/tmp", server: "stripe", tool: "list_customers", input: { limit: 2 }, read: true, confirmed: false });
  await app.call("shop.create", { email: "a@b.co" });
  expect(asked[1]).toMatchObject({ tool: "create_customer", read: false, confirmed: false });
  await app.call("shop.remove", { id: "cus_1" }, { confirmed: true });
  expect(asked[2]).toMatchObject({ tool: "delete_customer", read: false, confirmed: true });
  await app.call("shop.purge", {}, { confirmed: true });
  expect(asked[3]).toMatchObject({ tool: "delete_customer", confirmed: true });
  await expect(app.call("shop.sneak")).rejects.toMatchObject({ code: "INTERNAL", message: "NOT_READ_ONLY" });
  await app.close();
});

test("ctx.mcp turns a server's refusal into the error its caller reads", async () => {
  const refusing = definePlugin({
    methods: {
      read: method(z.object({}), (_, ctx) => ctx.mcp("stripe", "delete_customer"), { kind: "query" }),
      write: method(z.object({}), (_, ctx) => ctx.mcp("stripe", "delete_customer")),
    },
  });
  const refusals: Record<string, string> = { true: "NOT_READ_ONLY", false: "CONFIRM" };
  const runtime = await Runtime.open({
    dataDir: await mkdtemp(join(tmpdir(), "repogo-mcp-")),
    host: async (_plugin, _method, params) => ({ error: { code: refusals[String((params as TestMCPCall).read)], message: "refused" } }),
  });
  await runtime.add("refusing", refusing);
  await expect(runtime.call("refusing.read")).rejects.toMatchObject({ code: "FORBIDDEN", message: "stripe.delete_customer changes data, so it runs from a mutation, not a query." });
  await expect(runtime.call("refusing.write")).rejects.toMatchObject({ code: "FORBIDDEN", message: "stripe.delete_customer may destroy data, so it runs only from a method with a confirm." });
  runtime.close();
});

test("ctx.mcp needs the host", async () => {
  const lone = definePlugin({ methods: { read: method(z.object({}), (_, ctx) => ctx.mcp("stripe", "list_customers"), { kind: "query" }) } });
  const app = await testRuntime({ lone });
  await expect(app.call("lone.read")).rejects.toMatchObject({ code: "INTERNAL", message: "ctx.mcp runs only under the RepoGo host." });
  await app.close();
});
