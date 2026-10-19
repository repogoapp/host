import { afterEach, expect, test } from "bun:test";
import { definePlugin, method, MethodError, z } from "../src";
import { captureConsole, record, releaseConsole, textLimit, type LogRecord } from "../src/logs";
import { testRuntime } from "../src/testing";

// A plugin writes console.log; each line is kept with the plugin, the method
// and the call it was written in, and so is an error a method throws.

afterEach(() => releaseConsole());

const notes = definePlugin({
  namespace: "notes",
  collections: { notes: z.object({ text: z.string() }) },
  methods: {
    add: method(z.object({ text: z.string() }), async (input, ctx) => {
      console.log("adding", { text: input.text });
      ctx.log("through ctx.log");
      const note = await ctx.collections.notes.create(input);
      console.warn("added", note.id);
      return note.id;
    }),
    count: method(z.object({}), async (_, ctx) => {
      console.debug("counting");
      return (await ctx.collections.notes.list()).length;
    }, { kind: "query" }),
    viaDigest: method(z.object({}), (_, ctx) => ctx.call("digest.summary"), { kind: "query" }),
    boom: method(z.object({}), () => { throw new MethodError("NOT_FOUND", "no such thing"); }),
    tidy: method(z.object({}), () => { console.info("tidied"); }),
    ask: method(z.object({}), () => { throw new MethodError("CONFIRM", "Really?"); }, { confirm: "Really?" }),
  },
  on: {
    "notes.created": async () => { console.log("handler saw a note"); },
  },
});

const digest = definePlugin({
  methods: {
    summary: method(z.object({}), async (_, ctx) => {
      console.log("summarizing");
      return `notes: ${await ctx.call("notes.count")}`;
    }, { kind: "query" }),
  },
});

const lines = (logs: LogRecord[]) => logs.map((l) => `${l.level} ${l.plugin} ${l.method} ${l.caller}: ${l.text}`);

test("a method's console lines carry the plugin, the method, the caller and one call id", async () => {
  const app = await testRuntime({ notes, digest });
  await app.call("notes.add", { text: "hi" }, { caller: "agent", chat: "A-7" });
  expect(lines(app.logs)).toEqual([
    "info notes notes.add agent: adding { text: 'hi' }",
    "info notes notes.add agent: through ctx.log",
    "info notes notes.add agent: handler saw a note",
    "warn notes notes.add agent: added " + app.logs[3]!.text.slice("added ".length),
  ]);
  expect(new Set(app.logs.map((l) => l.callId)).size).toBe(1);
  expect(app.logs[0]!.callId).toHaveLength(8);
  expect(app.logs[0]!.chatId).toBe("A-7");
  expect(app.logs[0]!.data).toEqual(["adding", { text: "hi" }]);
  await app.close();
});

test("a call into another plugin keeps the call id and names the code running", async () => {
  const app = await testRuntime({ notes, digest });
  await app.call("notes.viaDigest", {}, { caller: "card" });
  expect(lines(app.logs)).toEqual(["info digest digest.summary card: summarizing", "debug notes notes.count card: counting"]);
  expect(app.logs[0]!.callId).toBe(app.logs[1]!.callId);
  await app.close();
});

test("a cron's lines say so, and a thrown error is recorded with its stack", async () => {
  const app = await testRuntime({ notes });
  app.setCrons([{ plugin: "notes", method: "tidy", schedule: "* * * * *" }]);
  await app.tickCrons(app.manifest().plugins[0]!.crons[0]!.nextRunAt!);
  expect(lines(app.logs)).toEqual(["info notes notes.tidy cron: tidied"]);

  await expect(app.call("notes.boom", {}, { caller: "agent" })).rejects.toThrow("no such thing");
  const failed = app.logs[1]!;
  expect(failed.level).toBe("error");
  expect(failed.method).toBe("notes.boom");
  expect(failed.text).toContain("no such thing");
  expect((failed.data[0] as { stack: string }).stack).toContain("logs.test.ts");

  // A confirm is a question for the user, not a failure.
  await expect(app.call("notes.ask", {}, { confirmed: false })).rejects.toThrow("Really?");
  expect(app.logs).toHaveLength(2);
  await app.close();
});

test("an error nothing caught is laid to the run going at the time", async () => {
  const app = await testRuntime({
    late: definePlugin({
      methods: {
        go: method(z.object({}), async () => {
          // Thrown from a timer, where the run's async context is gone.
          setTimeout(() => record("error", [new Error("late")]), 0);
          await Bun.sleep(5);
        }),
      },
    }),
  });
  await app.call("late.go", {}, { caller: "cron" });
  expect(lines(app.logs)).toEqual([expect.stringMatching(/^error late late.go cron: Error: late/)]);
  record("warn", ["nobody's"]);
  expect(app.logs).toHaveLength(1);
  await app.close();
});

test("a line is cut at the text limit, and releasing puts the console back", async () => {
  const original = console.log;
  const kept: LogRecord[] = [];
  captureConsole((entry) => kept.push(entry));
  expect(console.log).not.toBe(original);
  record("info", ["x".repeat(textLimit + 10)], { plugin: "p" });
  expect(kept).toHaveLength(1);
  expect(kept[0]!.text).toHaveLength(textLimit);
  expect(kept[0]!.data).toEqual(["x".repeat(textLimit + 10)]);
  releaseConsole();
  expect(console.log).toBe(original);
});

test("ctx.agent starts a chat and answers once the turn is done, naming the chat", async () => {
  const asked: { method: string; params: any }[] = [];
  const waits = [
    { turn_id: "t1", state: "running" },
    { turn_id: "t1", chat_id: "claude:abc", state: "running" },
    { turn_id: "t1", chat_id: "claude:abc", state: "done", reply: "The build fails on a missing import." },
  ];
  const thinker = definePlugin({
    methods: {
      why: method(z.object({}), (_, ctx) => ctx.agent("Why does the build fail?", { agent: "claude", title: "Build" })),
      noAgent: method(z.object({}), (_, ctx) => ctx.agent("hi", {} as any)),
    },
  });
  const app = await testRuntime({ thinker }, {
    host: (method, params) => {
      asked.push({ method, params });
      if (method === "chats.start") return { turn_id: "t1", state: "running" };
      return waits.shift();
    },
  });
  expect(await app.call("thinker.why")).toEqual({ chatId: "claude:abc", text: "The build fails on a missing import." });
  expect(asked[0]).toEqual({ method: "chats.start", params: { cwd: expect.any(String), agent: "claude", title: "Build", turn: { prompt: "Why does the build fail?", config: {}, attachments: [] } } });
  expect(asked.slice(1).map((a) => a.method)).toEqual(["chats.wait", "chats.wait", "chats.wait"]);
  expect(asked[1]!.params).toEqual({ turn_id: "t1", timeout_ms: 50_000 });
  await expect(app.call("thinker.noAgent")).rejects.toThrow("needs the agent");
  await app.close();
});

test("ctx.agent gives up at its timeout and says which chat kept running", async () => {
  const thinker = definePlugin({
    methods: {
      why: method(z.object({}), (_, ctx) => ctx.agent("hi", { agent: "codex", timeout: 20 })),
    },
  });
  const app = await testRuntime({ thinker }, {
    host: (method) => method === "chats.start"
      ? { turn_id: "t2", state: "running" }
      : { turn_id: "t2", chat_id: "codex:xyz", state: "running" },
  });
  await expect(app.call("thinker.why")).rejects.toThrow("codex:xyz");
  await app.close();
});
