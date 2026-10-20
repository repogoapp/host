import { afterEach, expect, test } from "bun:test";
import type { Duration } from "../duration";
import { RunJournal } from "./journal";
import { runWorkflow, type RunContext } from "./runner";
import { MemoryStorage } from "./fixtures/memory-storage";
import { TestClock } from "./fixtures/clock";
import { stepPolicy } from "./policy";
import type { JSONValue } from "./events";

const identity = { workflow: "report", buildId: "sleep-a", input: null };
const journals: RunJournal[] = [];
const clocks: TestClock[] = [];
afterEach(async () => {
  for (const journal of journals.splice(0)) await journal.close();
  for (const clock of clocks.splice(0)) expect(clock.timers.size).toBe(0);
});
async function open(storage = new MemoryStorage(), clock = new TestClock()) {
  const journal = await RunJournal.recover(storage, clock);
  journals.push(journal);
  if (!clocks.includes(clock)) clocks.push(clock);
  return { journal, storage, clock };
}
const events = (storage: MemoryStorage) => storage.events.toString().trim().split("\n").map(line => JSON.parse(line));

test("a five-minute sleep checkpoints once, releases timers, and replays its prefix after reopen", async () => {
  const { journal, storage, clock } = await open();
  let prefix = 0;
  let suffix = 0;
  let handlers = 0;
  const handler = async (_: JSONValue, ctx: RunContext) => {
    handlers++;
    const value = await ctx.step.run("first", () => ++prefix);
    await ctx.sleep("5m");
    return ctx.step.run("second", () => { suffix++; return value + 1; });
  };
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ code: "WAITING", attempt: 0 });
  expect(journal.state.operationOrder).toEqual(["first", "@sleep:2"]);
  expect(journal.state.sleeps["@sleep:2"]).toEqual({ status: "waiting", durationMs: 300_000, wakeAt: new Date(clock.now() + 300_000).toISOString() });
  expect(clock.timers.size).toBe(0);
  expect(suffix).toBe(0);
  await journal.close();
  const reopened = (await open(storage, clock)).journal;
  clock.advance(299_999);
  const seq = reopened.state.lastSeq;
  await expect(runWorkflow(reopened, identity, handler)).rejects.toMatchObject({ code: "WAITING" });
  expect(reopened.state.lastSeq).toBe(seq);
  expect(handlers).toBe(1);
  clock.advance(1);
  expect(await runWorkflow(reopened, identity, handler)).toBe(2);
  expect(prefix).toBe(1);
  expect(suffix).toBe(1);
  expect(events(storage).filter(e => e.type === "sleep.scheduled")).toHaveLength(1);
  expect(events(storage).filter(e => e.type === "sleep.completed")).toHaveLength(1);
});

test("consecutive and looped sleeps get separate positional IDs and keep the saved deadline", async () => {
  const { journal, clock } = await open();
  let duration: Duration = "1s";
  const handler = async (_: JSONValue, ctx: RunContext) => {
    for (let i = 0; i < 2; i++) await ctx.sleep(duration);
    return "done";
  };
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ stepId: "@sleep:1" });
  duration = "2s";
  clock.advance(1000);
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ stepId: "@sleep:2" });
  expect(journal.state.sleeps["@sleep:1"].durationMs).toBe(1000);
  expect(journal.state.sleeps["@sleep:2"].durationMs).toBe(2000);
  clock.advance(2000);
  duration = -1;
  expect(await runWorkflow(journal, identity, handler)).toBe("done");
});

test("sleep completion survives a crash before the following callback starts", async () => {
  const { journal, storage, clock } = await open();
  const handler = async (_: JSONValue, ctx: RunContext) => { await ctx.sleep("1s"); return ctx.step.run("next", () => "done"); };
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ code: "WAITING" });
  clock.advance(1000);
  await journal.append({ type: "sleep.completed", sleepId: "@sleep:1" });
  await journal.close();
  const reopened = (await open(storage, clock)).journal;
  expect(await runWorkflow(reopened, identity, handler)).toBe("done");
  expect(events(storage).filter(e => e.type === "sleep.completed")).toHaveLength(1);
});

test("a query retry after a sleep preserves the sleep and the completed prefix", async () => {
  const { journal, clock, storage } = await open();
  let prefix = 0;
  let queries = 0;
  const handler = async (_: JSONValue, ctx: RunContext) => {
    await ctx.step.run("prefix", () => ++prefix);
    await ctx.sleep(1000);
    return ctx.step.run("query", ({ attempt }) => { queries++; if (attempt === 1) throw new Error("temporary"); return "done"; }, { kind: "query", attempts: 2, backoff: { initial: 100, max: 100 } });
  };
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ code: "WAITING" });
  clock.advance(1000);
  await expect(runWorkflow(journal, identity, handler, { random: () => 0.5 })).rejects.toMatchObject({ code: "WAITING", stepId: "query" });
  clock.advance(50);
  expect(await runWorkflow(journal, identity, handler)).toBe("done");
  expect(prefix).toBe(1);
  expect(queries).toBe(2);
  expect(events(storage).filter(e => e.type === "sleep.scheduled")).toHaveLength(1);
});

test.each([0, -1, NaN, Infinity, "0s", "bad", "366d"])("invalid initial sleep refuses later effects: %s", async duration => {
  const { journal } = await open(); let effects = 0;
  await expect(runWorkflow(journal, identity, async (_, ctx) => {
    await ctx.sleep(duration as Duration);
    return ctx.step.run("later", () => ++effects);
  })).rejects.toMatchObject({ code: "FAILED" });
  expect(effects).toBe(0);
  expect(journal.state.sleeps).toEqual({});
});

test.each(["omit", "replace", "reorder"])("shared replay order refuses changed operations: %s", async mode => {
  const { journal, clock } = await open();
  await expect(runWorkflow(journal, identity, async (_, ctx) => { await ctx.step.run("first", () => 1); await ctx.sleep("1s"); return 1; })).rejects.toMatchObject({ code: "WAITING" });
  clock.advance(1000);
  let effects = 0;
  await expect(runWorkflow(journal, identity, async (_, ctx) => {
    if (mode === "reorder") { await ctx.sleep("1s"); return 0; }
    await ctx.step.run("first", () => ++effects);
    if (mode === "replace") return ctx.step.run("wrong", () => ++effects);
    return 1;
  })).rejects.toMatchObject({ code: "FAILED" });
  expect(effects).toBe(0);
});

test.each(["nested", "concurrent", "unawaited", "caught"])("sleep uses the same sequential and sticky guards as steps: %s", async mode => {
  const { journal } = await open(); let effects = 0;
  const execution = runWorkflow(journal, identity, async (_, ctx) => {
    if (mode === "nested") return ctx.step.run("outer", async () => { try { await ctx.sleep("1s"); } catch {} return null; });
    if (mode === "concurrent") { await Promise.all([ctx.sleep("1s"), ctx.step.run("later", () => ++effects)]); return null; }
    if (mode === "unawaited") { void ctx.sleep("1s"); return null; }
    try { await ctx.sleep("1s"); } catch {}
    try { await ctx.step.run("later", () => ++effects); } catch {}
    return null;
  });
  await expect(execution).rejects.toMatchObject({ code: mode === "caught" ? "WAITING" : "FAILED" });
  expect(effects).toBe(0);
});

test("cancellation and a whole-run deadline stop a sleeping workflow before its next step", async () => {
  for (const mode of ["cancel", "expire"]) {
    const { journal, clock } = await open(); let effects = 0;
    const handler = async (_: JSONValue, ctx: RunContext) => { await ctx.sleep("5m"); return ctx.step.run("later", () => ++effects); };
    await expect(runWorkflow(journal, identity, handler, { timeout: "1m" })).rejects.toMatchObject({ code: "WAITING" });
    if (mode === "cancel") await journal.append({ type: "run.cancelRequested" });
    else clock.advance(60_000);
    await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ code: mode === "cancel" ? "CANCELLED" : "EXPIRED" });
    expect(effects).toBe(0);
  }
});

test("the reducer refuses early completion, forged sleep IDs, and work across a pending sleep", async () => {
  const { journal, clock } = await open();
  await journal.append({ type: "run.started", ...identity, timeoutMs: 600_000 });
  await expect(journal.append({ type: "sleep.scheduled", sleepId: "@sleep:2", durationMs: 1000 })).rejects.toMatchObject({ code: "INVALID_EVENT" });
  await journal.append({ type: "sleep.scheduled", sleepId: "@sleep:1", durationMs: 1000 });
  await expect(journal.append({ type: "sleep.completed", sleepId: "@sleep:1" })).rejects.toMatchObject({ code: "NOT_DUE" });
  await expect(journal.append({ type: "run.completed", result: null })).rejects.toMatchObject({ code: "INVALID_EVENT" });
  await expect(journal.append({ type: "step.started", stepId: "later", attempt: 1, policy: stepPolicy() })).rejects.toMatchObject({ code: "INVALID_EVENT" });
  await expect(journal.append({ type: "sleep.scheduled", sleepId: "@sleep:2", durationMs: 1000 })).rejects.toMatchObject({ code: "INVALID_EVENT" });
  clock.advance(1000);
  await journal.append({ type: "sleep.completed", sleepId: "@sleep:1" });
  await journal.append({ type: "run.completed", result: null });
});

test("user step names cannot collide with generated sleep IDs", async () => {
  const { journal } = await open(); let effects = 0;
  await expect(runWorkflow(journal, identity, async (_, ctx) => ctx.step.run("@sleep:1", () => ++effects))).rejects.toMatchObject({ code: "FAILED" });
  expect(effects).toBe(0);
});

test("sleep duration is anchored before storage acknowledgement latency", async () => {
  const clock = new TestClock(); const storage = new MemoryStorage();
  const append = storage.append.bind(storage);
  storage.append = async record => {
    await append(record);
    if (JSON.parse(new TextDecoder().decode(record)).type === "sleep.scheduled") clock.advance(60_000);
  };
  const { journal } = await open(storage, clock);
  const startedAt = clock.now();
  await expect(runWorkflow(journal, identity, async (_, ctx) => { await ctx.sleep("5m"); return null; })).rejects.toMatchObject({ code: "WAITING" });
  expect(journal.state.sleeps["@sleep:1"].wakeAt).toBe(new Date(startedAt + 300_000).toISOString());
});

test("clock rollback during completion keeps the original sleep waiting without poisoning storage", async () => {
  const { journal, clock } = await open();
  const handler = async (_: JSONValue, ctx: RunContext) => { await ctx.sleep(1000); return "done"; };
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ code: "WAITING" });
  clock.advance(1000);
  const append = journal.append.bind(journal); let rollback = true;
  journal.append = async event => {
    if (event.type === "sleep.completed" && rollback) { rollback = false; clock.advance(-1); }
    return append(event);
  };
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ code: "WAITING" });
  expect(journal.state.sleeps["@sleep:1"].status).toBe("waiting");
  clock.advance(1);
  expect(await runWorkflow(journal, identity, handler)).toBe("done");
});

test.each(["cancel", "expire"])("terminal control wins while the sleep event is flushing: %s", async mode => {
  const clock = new TestClock(); const storage = new MemoryStorage(); const cancel = new AbortController();
  const append = storage.append.bind(storage);
  storage.append = async record => {
    await append(record);
    if (JSON.parse(new TextDecoder().decode(record)).type === "sleep.scheduled") {
      if (mode === "cancel") cancel.abort(); else clock.advance(1000);
    }
  };
  const { journal } = await open(storage, clock); let effects = 0;
  await expect(runWorkflow(journal, identity, async (_, ctx) => {
    await ctx.sleep("5m"); return ctx.step.run("later", () => ++effects);
  }, { signal: cancel.signal, timeout: 1000 })).rejects.toMatchObject({ code: mode === "cancel" ? "CANCELLED" : "EXPIRED" });
  expect(effects).toBe(0);
});

test("a committed sleep completion with a failed acknowledgement is recovered without rescheduling", async () => {
  const { journal, clock, storage } = await open(); let effects = 0;
  const handler = async (_: JSONValue, ctx: RunContext) => { await ctx.sleep(1000); return ctx.step.run("next", () => ++effects); };
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ code: "WAITING" });
  clock.advance(1000);
  const append = storage.append.bind(storage); const fault = new Error("completion flush failed"); let fail = true;
  storage.append = async record => {
    await append(record);
    if (JSON.parse(new TextDecoder().decode(record)).type === "sleep.completed" && fail) { fail = false; throw fault; }
  };
  await expect(runWorkflow(journal, identity, handler)).rejects.toBe(fault);
  expect(effects).toBe(0);
  await journal.close();
  const reopened = (await open(storage, clock)).journal;
  expect(await runWorkflow(reopened, identity, handler)).toBe(1);
  expect(events(storage).filter(e => e.type === "sleep.scheduled")).toHaveLength(1);
  expect(events(storage).filter(e => e.type === "sleep.completed")).toHaveLength(1);
});

test("manual query retry reuses a completed sleep without waiting again", async () => {
  const { journal, clock } = await open();
  const handler = async (_: JSONValue, ctx: RunContext) => {
    await ctx.sleep(1000);
    return ctx.step.run("query", ({ attempt }) => { if (attempt === 1) throw new Error("temporary"); return attempt; }, { kind: "query" });
  };
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ code: "WAITING" });
  clock.advance(1000);
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ code: "FAILED" });
  await journal.append({ type: "run.retryRequested", stepId: "query" });
  expect(await runWorkflow(journal, identity, handler)).toBe(2);
});

test("a detached callback retains its attempt scope and cannot claim a sleep position", async () => {
  const { journal } = await open(); let effects = 0;
  const release = Promise.withResolvers<void>(); let detached!: Promise<void>;
  await expect(runWorkflow(journal, identity, async (_, ctx) => {
    await ctx.step.run("first", () => {
      detached = release.promise.then(() => ctx.sleep("1s"));
      void detached.catch(() => {});
      return 1;
    });
    release.resolve();
    await expect(detached).rejects.toMatchObject({ code: "USAGE" });
    return ctx.step.run("later", () => ++effects);
  })).rejects.toMatchObject({ code: "FAILED" });
  expect(journal.state.operationOrder).toEqual(["first"]);
  expect(journal.state.sleeps).toEqual({});
  expect(effects).toBe(0);
});

test("executor suspension after scheduling keeps the durable sleep for a later resume", async () => {
  const clock = new TestClock(); const storage = new MemoryStorage(); const suspend = new AbortController();
  const append = storage.append.bind(storage);
  storage.append = async record => {
    await append(record);
    if (JSON.parse(new TextDecoder().decode(record)).type === "sleep.scheduled") suspend.abort();
  };
  const { journal } = await open(storage, clock);
  const handler = async (_: JSONValue, ctx: RunContext) => { await ctx.sleep(1000); return "done"; };
  await expect(runWorkflow(journal, identity, handler, { suspend: suspend.signal })).rejects.toMatchObject({ code: "PAUSED" });
  expect(journal.state.cancelRequested).toBe(false);
  expect(journal.state.status).toBe("waiting");
  clock.advance(1000);
  expect(await runWorkflow(journal, identity, handler)).toBe("done");
});
