import { afterEach, expect, test } from "bun:test";
import { assertWritable } from "../tracking";
import { TestClock } from "./fixtures/clock";
import { MemoryStorage } from "./fixtures/memory-storage";
import { RunJournal } from "./journal";
import { NonRetryable, runWorkflow, type RunContext, type StepContext, type StepOptions } from "./runner";
import { stepPolicy } from "./policy";
import type { JSONValue } from "./events";

const identity = { workflow: "report", input: null };
const journals: RunJournal[] = [];
const clocks: TestClock[] = [];
const releases: (() => void)[] = [];
const executions: Promise<unknown>[] = [];
afterEach(async () => {
  for (const release of releases.splice(0)) release();
  await Promise.allSettled(executions.splice(0));
  for (const journal of journals.splice(0)) await journal.close().catch(() => {});
  for (const clock of clocks.splice(0)) expect(clock.timers.size).toBe(0);
});
async function open(storage = new MemoryStorage(), clock = new TestClock()) {
  const journal = await RunJournal.recover(storage, clock);
  journals.push(journal);
  if (!clocks.includes(clock)) clocks.push(clock);
  return { journal, storage, clock };
}
function tracked<T>(promise: Promise<T>): Promise<T> {
  executions.push(promise);
  void promise.catch(() => {});
  return promise;
}
function deferred<T>(cleanup: T) {
  const pending = Promise.withResolvers<T>();
  releases.push(() => pending.resolve(cleanup));
  return pending;
}
const query: StepOptions = { kind: "query", attempts: 3, timeout: 1000, backoff: { initial: 100, factor: 2, max: 150 } };

test("two failed attempts survive reopen and succeed on attempt three without repeating the completed prefix", async () => {
  const { journal, storage, clock } = await open();
  let prefixCalls = 0;
  let handlerCalls = 0;
  let samples = 0;
  const attempts: number[] = [];
  const handler = async (_: JSONValue, { step }: RunContext) => {
    handlerCalls += 1;
    await step.run("prefix", () => { prefixCalls += 1; return { saved: true }; });
    return await step.run("fetch", ({ attempt, attempts: total }) => {
      attempts.push(attempt);
      expect(total).toBe(3);
      if (attempt < 3) throw new Error("temporary");
      return { done: true };
    }, query);
  };
  const options = { random: () => { samples += 1; return 0.5; } };
  await expect(runWorkflow(journal, identity, handler, options)).rejects.toMatchObject({ code: "WAITING" });
  expect(journal.state.steps.fetch.nextAttemptAt).toBe(new Date(clock.now() + 50).toISOString());
  await journal.close();
  const reopened = (await open(storage, clock)).journal;
  const seq = reopened.state.lastSeq;
  await expect(runWorkflow(reopened, identity, handler, options)).rejects.toMatchObject({ code: "WAITING" });
  expect(reopened.state.lastSeq).toBe(seq);
  expect(handlerCalls).toBe(1);
  expect(samples).toBe(1);
  clock.advance(50);
  await expect(runWorkflow(reopened, identity, handler, options)).rejects.toMatchObject({ code: "WAITING" });
  expect(reopened.state.steps.fetch.nextAttemptAt).toBe(new Date(clock.now() + 75).toISOString());
  await reopened.close();
  clock.advance(75);
  const third = (await open(storage, clock)).journal;
  expect(await runWorkflow(third, identity, handler, options)).toEqual({ done: true });
  expect(attempts).toEqual([1, 2, 3]);
  expect(prefixCalls).toBe(1);
  expect(samples).toBe(2);
});

test("total attempts are bounded and mutation steps default to one invocation", async () => {
  const first = await open();
  let mutationCalls = 0;
  await expect(runWorkflow(first.journal, identity, async (_, { step }) => {
    return await step.run("write", () => { mutationCalls += 1; throw new Error("failed write"); });
  })).rejects.toMatchObject({ code: "FAILED" });
  expect(mutationCalls).toBe(1);
  expect(first.journal.state.status).toBe("failed");
  const second = await open();
  let calls = 0;
  const handler = async (_: JSONValue, { step }: RunContext) => await step.run("read", () => {
    calls += 1; throw new Error("still failing");
  }, { kind: "query", attempts: 2 });
  await expect(runWorkflow(second.journal, identity, handler, { random: () => 0 })).rejects.toMatchObject({ code: "WAITING" });
  await expect(runWorkflow(second.journal, identity, handler)).rejects.toMatchObject({ code: "FAILED" });
  expect(calls).toBe(2);
  expect(second.journal.state.steps.read.attempt).toBe(2);
});

test("NonRetryable ends a query immediately even with unused attempts", async () => {
  const { journal } = await open();
  await expect(runWorkflow(journal, identity, async (_, { step }) => {
    return await step.run("read", () => { throw new NonRetryable("not found"); }, { kind: "query", attempts: 5 });
  })).rejects.toMatchObject({ code: "FAILED", failure: { code: "NON_RETRYABLE" } });
  expect(journal.state.steps.read.attempt).toBe(1);
});

test.each([
  { attempts: 2 }, { kind: "query", attempts: 0 }, { kind: "query", attempts: 1.5 },
  { timeout: 0 }, { timeout: Infinity }, { backoff: { initial: 100, max: 10 } },
  { backoff: { factor: NaN } }, { retries: 3 },
])("invalid policy refuses callback dispatch: %j", async options => {
  const { journal } = await open();
  let dispatched = false;
  await expect(runWorkflow(journal, identity, async (_, { step }) => {
    return await step.run("invalid", () => { dispatched = true; return null; }, options as StepOptions);
  })).rejects.toMatchObject({ code: "FAILED" });
  expect(dispatched).toBe(false);
  expect(journal.state.steps.invalid).toBeUndefined();
});

test("a query timeout aborts, retains ownership until settlement, then discards the late result", async () => {
  const { journal, clock } = await open();
  const started = deferred<void>(undefined);
  const release = deferred("late");
  let context: StepContext | undefined;
  const handler = async (_: JSONValue, { step }: RunContext) => await step.run("read", async ctx => {
    context = ctx;
    if (ctx.attempt === 2) return "fresh";
    started.resolve();
    return await release.promise;
  }, { kind: "query", attempts: 2, timeout: 10 });
  const execution = tracked(runWorkflow(journal, identity, handler, { random: () => 0 }));
  await started.promise;
  clock.advance(10);
  expect(context!.signal.aborted).toBe(true);
  expect(journal.state.steps.read.status).toBe("running");
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ code: "BUSY" });
  release.resolve("late");
  await expect(execution).rejects.toMatchObject({ code: "WAITING" });
  expect(journal.state.steps.read.result).toBeNull();
  expect(await runWorkflow(journal, identity, handler)).toBe("fresh");
  expect(journal.state.steps.read.attempt).toBe(2);
});

test("a timed-out mutation is unknown and never accepts a late success", async () => {
  const { journal, clock } = await open();
  const started = deferred<void>(undefined);
  const release = deferred(null);
  const execution = tracked(runWorkflow(journal, identity, async (_, { step }) => await step.run("write", async () => {
    started.resolve(); return await release.promise;
  }, { timeout: 10 })));
  await started.promise;
  clock.advance(10);
  release.resolve(null);
  await expect(execution).rejects.toMatchObject({ code: "UNKNOWN", failure: { code: "TIMEOUT" } });
  expect(journal.state.steps.write.status).toBe("unknown");
});

test("whole-run expiry is persisted and stops a reopened waiting run before its handler", async () => {
  const { journal, storage, clock } = await open();
  await expect(runWorkflow(journal, identity, async (_, { step }) => await step.run("read", () => {
    throw new Error("temporary");
  }, query), { timeout: 20, random: () => 0.5 })).rejects.toMatchObject({ code: "WAITING" });
  await journal.close();
  clock.advance(20);
  const reopened = (await open(storage, clock)).journal;
  await expect(runWorkflow(reopened, identity, () => { throw new Error("must not dispatch"); }, { timeout: "365d" })).rejects.toMatchObject({ code: "EXPIRED" });
  expect(reopened.state.status).toBe("expired");
});

test("cancellation is flushed before aborting and drains an active callback before releasing ownership", async () => {
  const { journal } = await open();
  const started = deferred<void>(undefined);
  const aborted = deferred<void>(undefined);
  const release = deferred(null);
  const controller = new AbortController();
  const execution = tracked(runWorkflow(journal, identity, async (_, { step }) => await step.run("read", async ({ signal }) => {
    signal.addEventListener("abort", () => {
      expect(journal.state.cancelRequested).toBe(true);
      aborted.resolve();
    }, { once: true });
    started.resolve(); return await release.promise;
  }, query), { signal: controller.signal }));
  await started.promise;
  controller.abort();
  await aborted.promise;
  await expect(runWorkflow(journal, identity, () => null)).rejects.toMatchObject({ code: "BUSY" });
  release.resolve(null);
  await expect(execution).rejects.toMatchObject({ code: "CANCELLED" });
  expect(journal.state.status).toBe("cancelled");
  const seq = journal.state.lastSeq;
  controller.abort();
  expect(journal.state.lastSeq).toBe(seq);
});

test("recovered cancellation wins before dispatch and query steps cannot write SDK data", async () => {
  const first = await open();
  await first.journal.append({ type: "run.started", ...identity, timeoutMs: 30 * 86_400_000 });
  await first.journal.append({ type: "run.cancelRequested" });
  await expect(runWorkflow(first.journal, identity, () => { throw new Error("must not dispatch"); })).rejects.toMatchObject({ code: "CANCELLED" });
  expect(first.journal.state.status).toBe("cancelled");
  const second = await open();
  await expect(runWorkflow(second.journal, identity, async (_, { step }) => await step.run("read", () => {
    assertWritable("example.items"); return null;
  }, { kind: "query" }))).rejects.toThrow("can't change data");
});

test("an interrupted query consumes its attempt and schedules a retry from recorded policy", async () => {
  const { journal, storage, clock } = await open();
  const policy = stepPolicy({ kind: "query", attempts: 2 });
  await journal.append({ type: "run.started", ...identity, timeoutMs: 30 * 86_400_000 });
  await journal.append({ type: "step.started", stepId: "read", attempt: 1, policy });
  await journal.close();
  const reopened = (await open(storage, clock)).journal;
  await expect(runWorkflow(reopened, identity, () => { throw new Error("must not dispatch"); }, { random: () => 0 })).rejects.toMatchObject({ code: "WAITING" });
  expect(reopened.state.steps.read.attempt).toBe(1);
  expect(await runWorkflow(reopened, identity, async (_, { step }) => await step.run("read", ({ attempt }) => attempt, { kind: "query", attempts: 2 }))).toBe(2);
});

test("an interrupted last query attempt is failed rather than reissued", async () => {
  const { journal } = await open();
  await journal.append({ type: "run.started", ...identity, timeoutMs: 30 * 86_400_000 });
  await journal.append({ type: "step.started", stepId: "read", attempt: 1, policy: stepPolicy({ kind: "query" }) });
  await expect(runWorkflow(journal, identity, () => { throw new Error("must not dispatch"); })).rejects.toMatchObject({ code: "FAILED", failure: { code: "INTERRUPTED" } });
  expect(journal.state.steps.read.attempt).toBe(1);
});

test("retry policies cannot change between attempts and swallowed suspension remains waiting", async () => {
  const { journal } = await open();
  await expect(runWorkflow(journal, identity, async (_, { step }) => {
    await step.run("read", () => { throw new Error("temporary"); }, { kind: "query", attempts: 2 }).catch(() => {});
    return "handler swallowed waiting";
  }, { random: () => 0 })).rejects.toMatchObject({ code: "WAITING" });
  expect(journal.state.status).toBe("waiting");
  await expect(runWorkflow(journal, identity, async (_, { step }) => await step.run("read", () => null, { kind: "query", attempts: 3 }))).rejects.toMatchObject({ code: "FAILED", failure: { code: "REPLAY" } });
});

test("zero jitter remains valid even if storage acknowledgement advances the clock", async () => {
  const clock = new TestClock();
  class SlowStorage extends MemoryStorage {
    async append(record: Uint8Array) { clock.advance(10); await super.append(record); }
  }
  const { journal } = await open(new SlowStorage(), clock);
  await expect(runWorkflow(journal, identity, async (_, { step }) => await step.run("read", () => {
    throw new Error("temporary");
  }, query), { random: () => 0 })).rejects.toMatchObject({ code: "WAITING" });
  expect(journal.state.status).toBe("waiting");
  expect(Date.parse(journal.state.steps.read.nextAttemptAt!)).toBeLessThanOrEqual(clock.now());
});

test("a backward wall-clock jump at dispatch yields waiting without poisoning the journal", async () => {
  const { journal, clock } = await open();
  let calls = 0;
  const handler = async (_: JSONValue, { step }: RunContext) => await step.run("read", () => {
    calls += 1;
    if (calls === 1) throw new Error("temporary");
    return calls;
  }, { kind: "query", attempts: 2 });
  await expect(runWorkflow(journal, identity, handler, { random: () => 0 })).rejects.toMatchObject({ code: "WAITING" });
  const append = journal.append.bind(journal);
  let jump = true;
  journal.append = event => {
    if (event.type === "step.started" && event.attempt === 2 && jump) {
      jump = false;
      clock.advance(-1);
    }
    return append(event);
  };
  const seq = journal.state.lastSeq;
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ code: "WAITING" });
  expect(journal.state.lastSeq).toBe(seq);
  expect(calls).toBe(1);
  clock.advance(1);
  expect(await runWorkflow(journal, identity, handler)).toBe(2);
});

test("expiry aborts an active query without allowing its late result to complete the run", async () => {
  const { journal, clock } = await open();
  const started = deferred<void>(undefined);
  const release = deferred(null);
  let signal: AbortSignal | undefined;
  const execution = tracked(runWorkflow(journal, identity, async (_, { step }) => await step.run("read", async ctx => {
    signal = ctx.signal; started.resolve(); return await release.promise;
  }, query), { timeout: 10 }));
  await started.promise;
  clock.advance(10);
  expect(signal!.aborted).toBe(true);
  await expect(runWorkflow(journal, identity, () => null)).rejects.toMatchObject({ code: "BUSY" });
  release.resolve(null);
  await expect(execution).rejects.toMatchObject({ code: "EXPIRED" });
  expect(journal.state.status).toBe("expired");
  expect(journal.state.steps.read.result).toBeNull();
});

test("an uncertainty marker survives a crash before the run's terminal event", async () => {
  const { journal, storage, clock } = await open();
  await journal.append({ type: "run.started", ...identity, timeoutMs: 30 * 86_400_000 });
  await journal.append({ type: "step.started", stepId: "read", attempt: 1, policy: stepPolicy() });
  await journal.append({ type: "step.unknown", stepId: "read", attempt: 1, error: { code: "INVALID_RESULT", message: "Invalid result" } });
  await journal.close();
  const reopened = (await open(storage, clock)).journal;
  await expect(runWorkflow(reopened, identity, () => { throw new Error("must not dispatch"); })).rejects.toMatchObject({ code: "UNKNOWN", failure: { code: "INVALID_RESULT" } });
  expect(reopened.state.status).toBe("unknown");
});

test("start-write latency does not consume the callback's per-attempt timeout", async () => {
  const clock = new TestClock();
  class SlowStart extends MemoryStorage {
    async append(record: Uint8Array) {
      if (JSON.parse(Buffer.from(record).toString()).type === "step.started") clock.advance(100);
      await super.append(record);
    }
  }
  const { journal } = await open(new SlowStart(), clock);
  let invoked = false;
  expect(await runWorkflow(journal, identity, async (_, { step }) => await step.run("write", () => {
    invoked = true; return "saved";
  }, { timeout: 1 }))).toBe("saved");
  expect(invoked).toBe(true);
});

test.each(["cancelled", "expired"])("a %s run never dispatches a step whose start is still flushing", async reason => {
  const clock = new TestClock();
  const controller = new AbortController();
  class StopDuringStart extends MemoryStorage {
    async append(record: Uint8Array) {
      if (JSON.parse(Buffer.from(record).toString()).type === "step.started") {
        if (reason === "cancelled") controller.abort();
        else clock.advance(10);
      }
      await super.append(record);
    }
  }
  const { journal } = await open(new StopDuringStart(), clock);
  let invoked = false;
  const handler = async (_: JSONValue, { step }: RunContext) => await step.run("write", () => { invoked = true; return null; });
  const options = { timeout: 10, signal: controller.signal };
  const first = await runWorkflow(journal, identity, handler, options).catch(error => error);
  expect(first.code).toBe(reason === "cancelled" ? "CANCELLED" : "EXPIRED");
  expect(invoked).toBe(false);
  expect(journal.state.steps.write.status).toBe("failed");
  expect(journal.state.status).toBe(reason);
  const replay = await runWorkflow(journal, identity, handler, options).catch(error => error);
  expect(replay.code).toBe(first.code);
  expect(replay.message).toBe(first.message);
});

test("the journal requires retry policy and run lifetime before accepting start events", async () => {
  const { journal } = await open();
  await expect(journal.append({ type: "run.started", ...identity } as never)).rejects.toMatchObject({ code: "INVALID_EVENT" });
  expect(journal.state.lastSeq).toBe(0);
  await journal.append({ type: "run.started", ...identity, timeoutMs: 1000 });
  await expect(journal.append({ type: "step.started", stepId: "unbounded", attempt: 1 } as never)).rejects.toMatchObject({ code: "INVALID_EVENT" });
  expect(journal.state.lastSeq).toBe(1);
});

test("invalid query output fails without repeating a deterministic result error", async () => {
  const { journal } = await open();
  let calls = 0;
  await expect(runWorkflow(journal, identity, async (_, { step }) => await step.run("read", () => {
    calls += 1; return undefined as never;
  }, query))).rejects.toMatchObject({ code: "FAILED", failure: { code: "INVALID_RESULT" } });
  expect(calls).toBe(1);
  expect(journal.state.steps.read.status).toBe("failed");
});

test("manual retry extends only the failed query budget and preserves the completed prefix", async () => {
  const { journal } = await open();
  let prefix = 0;
  let succeed = false;
  const handler = async (_: JSONValue, { step }: RunContext) => {
    await step.run("prefix", () => ++prefix);
    return step.run("query", ({ attempt, attempts }) => {
      if (!succeed) throw new Error("temporary");
      return { attempt, attempts };
    }, { kind: "query" });
  };
  await expect(runWorkflow(journal, identity, handler)).rejects.toMatchObject({ code: "FAILED" });
  await journal.append({ type: "run.retryRequested", stepId: "query" });
  succeed = true;
  expect(await runWorkflow(journal, identity, handler)).toEqual({ attempt: 2, attempts: 2 });
  expect(prefix).toBe(1);
});

test.each(["query", "mutation"] as const)("executor suspension preserves a finished %s result for replay", async kind => {
  const { journal, storage, clock } = await open();
  const started = deferred(false);
  const suspend = new AbortController();
  let calls = 0;
  const handler = async (_: JSONValue, { step }: RunContext) => step.run("work", async ({ signal }) => {
    calls++;
    started.resolve(true);
    await new Promise<void>(resolve => signal.addEventListener("abort", () => resolve(), { once: true }));
    return "saved";
  }, { kind });
  const execution = tracked(runWorkflow(journal, identity, handler, { suspend: suspend.signal }));
  await started.promise;
  suspend.abort();
  await expect(execution).rejects.toMatchObject({ code: "PAUSED" });
  expect(journal.state.cancelRequested).toBe(false);
  expect(journal.state.steps.work.status).toBe("completed");
  await journal.close();
  const reopened = (await open(storage, clock)).journal;
  expect(await runWorkflow(reopened, identity, handler)).toBe("saved");
  expect(calls).toBe(1);
});

test("an active executor expires across a long environment sleep before accepting a late result", async () => {
  const { journal, clock } = await open();
  const started = deferred(false);
  const finish = deferred("late");
  const execution = tracked(runWorkflow(journal, identity, async (_, { step }) => step.run("query", () => {
    started.resolve(true); return finish.promise;
  }, { kind: "query", timeout: "30d" }), { maxOffline: "14d", timeout: "30d" }));
  await started.promise;
  clock.advance(14 * 86400_000);
  finish.resolve("late");
  await expect(execution).rejects.toMatchObject({ code: "EXPIRED" });
  expect(journal.state.expiryReason).toContain("offline limit");
});
