import { afterEach, expect, test } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { stepPolicy } from "./policy";
import { RunJournal } from "./journal";
import { runWorkflow } from "./runner";
import type { JSONValue } from "./events";
import { TestClock } from "./fixtures/clock";
import { describe } from "./summary";
import type { JournalStorage } from "./storage";

const journalPolicy = stepPolicy();
const identity = { workflow: "report", input: { customer: "one" } };
const journals: RunJournal[] = [];
const directories: string[] = [];
const children: ReturnType<typeof Bun.spawn>[] = [];
afterEach(async () => {
  for (const child of children.splice(0)) { child.kill(); await child.exited; }
  for (const journal of journals.splice(0)) await journal.close().catch(() => {});
  for (const directory of directories.splice(0)) await rm(directory, { force: true, recursive: true });
});
async function open(directory?: string) {
  if (!directory) {
    directory = await mkdtemp(join(tmpdir(), "repogo-runner-"));
    directories.push(directory);
  }
  const journal = await RunJournal.open(directory);
  journals.push(journal);
  return { journal, directory };
}
async function prefix(journal: RunJournal, names: string[]) {
  await journal.append({ type: "run.started", ...identity, timeoutMs: 30 * 86_400_000 });
  for (const stepId of names) {
    await journal.append({ type: "step.started", stepId, attempt: 1, policy: journalPolicy });
    await journal.append({ type: "step.completed", stepId, attempt: 1, result: { saved: stepId } });
  }
}

test.each(["constructor", "toString", "__proto__"])("step details preserve the kind of %s", async name => {
  const { journal } = await open();
  await runWorkflow(journal, identity, async (_, { step }) => step.run(name, () => "saved"));
  expect(describe(journal.state).steps).toMatchObject([{ id: name, kind: "step", status: "completed" }]);
});

test("sequential callbacks see durable starts and return isolated saved results", async () => {
  const { journal } = await open();
  const result = await runWorkflow(journal, identity, async (input, { step }) => {
    expect(input).toEqual(identity.input);
    const first = await step.run("first", () => {
      expect(journal.state.steps.first?.status).toBe("running");
      return { count: 1 };
    });
    first.count = 9;
    return await step.run("second", () => ({ count: first.count + 1 }));
  });
  expect(result).toEqual({ count: 10 });
  expect(journal.state.steps.first?.result).toEqual({ count: 1 });
  expect(journal.state.status).toBe("completed");
  expect(journal.state.lastSeq).toBe(6);
});

test("reopening between steps reuses completed results then executes only the new step", async () => {
  const { journal, directory } = await open();
  await prefix(journal, ["first"]);
  await journal.close();
  const reopened = (await open(directory)).journal;
  let calls = 0;
  const result = await runWorkflow(reopened, identity, async (_, { step }) => {
    const first = await step.run<{ saved: string }>("first", () => { throw new Error("must not repeat"); });
    return await step.run("second", () => { calls += 1; return first; });
  });
  expect(result).toEqual({ saved: "first" });
  expect(calls).toBe(1);
  expect(reopened.state.operationOrder).toEqual(["first", "second"]);
});

test("an interrupted callback becomes unknown before any handler or callback repeats", async () => {
  const { journal, directory } = await open();
  await prefix(journal, ["first"]);
  await journal.append({ type: "step.started", stepId: "second", attempt: 1, policy: journalPolicy });
  await journal.close();
  const reopened = (await open(directory)).journal;
  let invoked = false;
  await expect(runWorkflow(reopened, identity, () => { invoked = true; return null; })).rejects.toMatchObject({ code: "UNKNOWN" });
  expect(invoked).toBe(false);
  expect(reopened.state.steps.first?.result).toEqual({ saved: "first" });
  expect(reopened.state.steps.second?.status).toBe("running");
  expect(reopened.state.status).toBe("unknown");
});

test("a completed run returns its recorded result without invoking the handler", async () => {
  const { journal } = await open();
  await runWorkflow(journal, identity, () => ({ count: 1 }));
  const result = await runWorkflow(journal, identity, () => { throw new Error("must not repeat"); });
  (result as { count: number }).count = 9;
  expect(journal.state.result).toEqual({ count: 1 });
  expect(journal.state.lastSeq).toBe(2);
});

test.each([
  { ...identity, workflow: "other" },
  { ...identity, input: { customer: "two" } },
])("identity changes refuse execution without changing history: %j", async changed => {
  const { journal } = await open();
  await prefix(journal, []);
  await expect(runWorkflow(journal, changed, () => null)).rejects.toMatchObject({ code: "IDENTITY" });
  expect(journal.state.lastSeq).toBe(1);
});

// Code that no longer matches the recorded steps was edited: the run waits for Restart or Cancel.
test.each(["reordered", "omitted"])("%s steps put the run in attention without repeating callbacks", async mode => {
  const { journal } = await open();
  await prefix(journal, ["10", "2"]);
  await expect(runWorkflow(journal, identity, async (_, { step }) => {
    if (mode === "reordered") return await step.run("2", () => { throw new Error("must not repeat"); });
    await step.run("10", () => null);
    return null;
  })).rejects.toMatchObject({ code: "ATTENTION", failure: { code: "CHANGED" } });
  expect(journal.state.status).toBe("attention");
  expect(journal.state.error?.message).toContain("The workflow changed since this run started");
  // Running it again changes nothing until it restarts.
  await expect(runWorkflow(journal, identity, () => null)).rejects.toMatchObject({ code: "ATTENTION" });
  expect(journal.state.status).toBe("attention");
});

test("a duplicate step name fails the run: a bug, not an edit", async () => {
  const { journal } = await open();
  await prefix(journal, ["10", "2"]);
  await expect(runWorkflow(journal, identity, async (_, { step }) => {
    await step.run("10", () => null);
    return await step.run("10", () => null);
  })).rejects.toMatchObject({ code: "FAILED", failure: { code: "REPLAY" } });
  expect(journal.state.status).toBe("failed");
});

test("an edit that keeps the recorded steps continues on the new code", async () => {
  const { journal } = await open();
  await prefix(journal, ["fetch"]);
  const result = await runWorkflow(journal, identity, async (_, { step }) => {
    const saved = await step.run<JSONValue>("fetch", () => { throw new Error("must not repeat"); });
    return await step.run("report", () => ({ from: saved, version: "edited" }));
  });
  expect(result).toEqual({ from: { saved: "fetch" }, version: "edited" });
  expect(journal.state.status).toBe("completed");
});

test("restart starts a run that needs attention over on the current code, with a fresh deadline", async () => {
  const directory = await mkdtemp(join(tmpdir(), "repogo-runner-"));
  directories.push(directory);
  const clock = new TestClock();
  const journal = await RunJournal.open(directory, clock);
  journals.push(journal);
  await prefix(journal, ["old"]);
  await expect(runWorkflow(journal, identity, async (_, { step }) => step.run("new", () => null))).rejects.toMatchObject({ code: "ATTENTION" });
  const deadline = journal.state.deadlineAt!;
  clock.advance(60_000);
  await journal.append({ type: "run.restarted" });
  expect(journal.state).toMatchObject({ status: "running", operationOrder: [], steps: {}, error: null });
  expect(Date.parse(journal.state.deadlineAt!) - Date.parse(deadline)).toBe(60_000);
  let ran = 0;
  expect(await runWorkflow(journal, identity, async (_, { step }) => step.run("new", () => ++ran))).toBe(1);
  expect(journal.state.status).toBe("completed");
});

test("a caught callback failure still fails the run and prevents subsequent effects", async () => {
  const { journal } = await open();
  let later = false;
  await expect(runWorkflow(journal, identity, async (_, { step }) => {
    await step.run("bad", () => { throw new Error("service failed"); }).catch(() => {});
    await step.run("later", () => { later = true; return null; }).catch(() => {});
    return null;
  })).rejects.toThrow("service failed");
  expect(later).toBe(false);
  expect(journal.state.steps.bad?.status).toBe("failed");
  expect(journal.state.status).toBe("failed");
});

test("concurrent executors and overlapping steps cannot dispatch duplicate work", async () => {
  const { journal } = await open();
  const started = Promise.withResolvers<void>();
  const finish = Promise.withResolvers<null>();
  const executing = runWorkflow(journal, identity, async (_, { step }) => {
    const first = step.run("first", () => { started.resolve(); return finish.promise; });
    await started.promise;
    await step.run("second", () => { throw new Error("must not execute"); }).catch(() => {});
    return await first;
  });
  await started.promise;
  await expect(runWorkflow(journal, identity, () => null)).rejects.toMatchObject({ code: "BUSY" });
  finish.resolve(null);
  await expect(executing).rejects.toMatchObject({ code: "FAILED", failure: { code: "USAGE" } });
  expect(journal.state.steps.first?.status).toBe("completed");
  expect(journal.state.steps.second).toBeUndefined();
});

test("nested steps are refused even when their error is caught", async () => {
  const { journal } = await open();
  await expect(runWorkflow(journal, identity, async (_, { step }) => {
    return await step.run("outer", async () => {
      await step.run("inner", () => null).catch(() => {});
      return null;
    });
  })).rejects.toMatchObject({ code: "FAILED", failure: { code: "USAGE" } });
  expect(journal.state.steps.inner).toBeUndefined();
  expect(journal.state.status).toBe("failed");
});

test("an unawaited callback must settle before the run fails", async () => {
  const { journal } = await open();
  const started = Promise.withResolvers<void>();
  const finish = Promise.withResolvers<null>();
  const executing = runWorkflow(journal, identity, async (_, { step }) => {
    void step.run("first", () => { started.resolve(); return finish.promise; });
    await started.promise;
    return null;
  });
  await started.promise;
  expect(journal.state.status).toBe("running");
  finish.resolve(null);
  await expect(executing).rejects.toMatchObject({ code: "FAILED", failure: { code: "USAGE" } });
  expect(journal.state.steps.first?.status).toBe("completed");
  expect(journal.state.status).toBe("failed");
});

test("non-JSON callback results mark uncertain effects and never continue", async () => {
  const { journal } = await open();
  await expect(runWorkflow(journal, identity, async (_, { step }) => {
    await step.run("effect", () => undefined as never).catch(() => {});
    return null;
  })).rejects.toMatchObject({ code: "UNKNOWN" });
  expect(journal.state.status).toBe("unknown");
});

test("non-JSON workflow results and non-Error throws fail explicitly", async () => {
  const first = (await open()).journal;
  await expect(runWorkflow(first, identity, () => undefined as never)).rejects.toMatchObject({ code: "FAILED", failure: { code: "USAGE" } });
  expect(first.state.status).toBe("failed");
  const second = (await open()).journal;
  await expect(runWorkflow(second, identity, () => { throw null; })).rejects.toThrow("null");
  expect(second.state.status).toBe("failed");
});

test("completion flush failure leaves interrupted history and refuses later effects", async () => {
  let bytes = Buffer.alloc(0);
  const storage: JournalStorage = {
    async readEvents() { return bytes; },
    async append(record) {
      if (JSON.parse(Buffer.from(record).toString()).type === "step.completed") throw new Error("flush failed");
      bytes = Buffer.concat([bytes, Buffer.from(record)]);
    },
    async truncateEvents(size) { bytes = bytes.subarray(0, size); },
    async replaceSnapshot() {},
    async close() {},
  };
  const journal = await RunJournal.recover(storage, { now: () => Date.parse("2026-01-01T00:00:00Z"), schedule: () => () => {} });
  journals.push(journal);
  let later = false;
  await expect(runWorkflow(journal, identity, async (_, { step }) => {
    await step.run("effect", () => null).catch(() => {});
    await step.run("later", () => { later = true; return null; }).catch(() => {});
    return null;
  })).rejects.toThrow("flush failed");
  expect(later).toBe(false);
  expect(journal.state.steps.effect?.status).toBe("running");
  expect(journal.state.status).toBe("running");
});


test("a killed executor preserves its completed step and never repeats interrupted effects", async () => {
  const { journal, directory } = await open();
  await journal.close();
  const child = Bun.spawn([process.execPath, fileURLToPath(new URL("fixtures/runner-owner.ts", import.meta.url)), directory], {
    stdin: "pipe", stdout: "pipe", stderr: "pipe",
  });
  children.push(child);
  const reader = child.stdout.getReader();
  let ready = "";
  while (!ready.includes("ready\n")) {
    const chunk = await reader.read();
    if (chunk.done) throw new Error(`Executor exited before readiness: ${await new Response(child.stderr).text()}`);
    ready += new TextDecoder().decode(chunk.value);
  }
  reader.releaseLock();
  child.kill("SIGKILL");
  await child.exited;
  children.splice(children.indexOf(child), 1);
  const reopened = (await open(directory)).journal;
  expect(reopened.state.steps.first?.result).toEqual({ saved: true });
  expect(reopened.state.steps.second?.status).toBe("running");
  await expect(runWorkflow(reopened, identity, () => { throw new Error("must not repeat"); })).rejects.toMatchObject({ code: "UNKNOWN" });
  expect(reopened.state.status).toBe("unknown");
});

test("a failed step recovered before run failure cannot be dispatched again", async () => {
  const { journal, directory } = await open();
  await prefix(journal, []);
  await journal.append({ type: "step.started", stepId: "bad", attempt: 1, policy: journalPolicy });
  await journal.append({ type: "step.failed", stepId: "bad", attempt: 1, error: { message: "service failed", code: "STEP" } });
  await journal.close();
  const reopened = (await open(directory)).journal;
  let invoked = false;
  await expect(runWorkflow(reopened, identity, () => { invoked = true; return null; })).rejects.toMatchObject({ code: "FAILED", failure: { code: "STEP" } });
  expect(invoked).toBe(false);
  expect(reopened.state.status).toBe("failed");
  await expect(runWorkflow(reopened, identity, () => { invoked = true; return null; })).rejects.toMatchObject({ code: "FAILED" });
  expect(invoked).toBe(false);
});

test("waiting retries are preserved for a scheduler instead of being executed early", async () => {
  const { journal } = await open();
  await prefix(journal, []);
  await journal.append({ type: "step.started", stepId: "waiting", attempt: 1, policy: stepPolicy({ kind: "query", attempts: 2 }) });
  await journal.append({
    type: "step.retryScheduled", stepId: "waiting", attempt: 1,
    error: { message: "try later" }, delayMs: 60_000,
  });
  const before = journal.state;
  await expect(runWorkflow(journal, identity, () => { throw new Error("must not dispatch"); })).rejects.toMatchObject({ code: "WAITING" });
  expect(journal.state).toEqual(before);
});

test("late calls to a retained step context cannot write or dispatch", async () => {
  const { journal } = await open();
  let saved: import("./runner").WorkflowSteps | undefined;
  await runWorkflow(journal, identity, (_, { step }) => { saved = step; return null; });
  const seq = journal.state.lastSeq;
  await expect(saved!.run("late", () => { throw new Error("must not execute"); })).rejects.toMatchObject({ code: "USAGE" });
  expect(journal.state.lastSeq).toBe(seq);
});

test("first execution and replay both see normalized persisted JSON", async () => {
  const { journal, directory } = await open();
  await prefix(journal, []);
  const negative = { number: -0 };
  await runWorkflow(journal, identity, async (_, { step }) => {
    const value = await step.run("value", () => negative);
    expect(Object.is(value.number, -0)).toBe(false);
    return value;
  });
  await journal.close();
  const reopened = (await open(directory)).journal;
  const result = await runWorkflow<{ number: number }>(reopened, identity, () => { throw new Error("must not execute"); });
  expect(result).toEqual({ number: 0 });
});

test("oversized results stop effects while preserving room to record uncertainty", async () => {
  const { journal } = await open();
  await expect(runWorkflow(journal, identity, async (_, { step }) => {
    return await step.run("large", () => "x".repeat(1024 * 1024));
  })).rejects.toMatchObject({ code: "UNKNOWN" });
  expect(journal.state.status).toBe("unknown");
  expect(journal.state.steps.large?.status).toBe("unknown");
});

test("an oversized final result fails durably instead of rerunning the handler", async () => {
  const { journal } = await open();
  await expect(runWorkflow(journal, identity, () => "x".repeat(1024 * 1024))).rejects.toMatchObject({ code: "FAILED" });
  expect(journal.state.status).toBe("failed");
});

test("Promise.all refuses overlap before either callback dispatches", async () => {
  const { journal } = await open();
  let calls = 0;
  await expect(runWorkflow(journal, identity, async (_, { step }) => {
    return await Promise.all([
      step.run("first", () => { calls += 1; return null; }),
      step.run("second", () => { calls += 1; return null; }),
    ]);
  })).rejects.toMatchObject({ code: "FAILED", failure: { code: "USAGE" } });
  expect(calls).toBe(0);
  expect(journal.state.steps.first?.status).toBe("failed");
  expect(journal.state.steps.second).toBeUndefined();
});

test("returning without awaiting a step stops it before dispatch when possible", async () => {
  const { journal } = await open();
  let invoked = false;
  await expect(runWorkflow(journal, identity, (_, { step }) => {
    void step.run("first", () => { invoked = true; return null; });
    return null;
  })).rejects.toMatchObject({ code: "FAILED", failure: { code: "USAGE" } });
  expect(invoked).toBe(false);
  expect(journal.state.steps.first?.status).toBe("failed");
});

test("terminal failure reuses the same persisted error contract", async () => {
  const { journal } = await open();
  const execute = () => runWorkflow(journal, identity, async (_, { step }) => {
    return await step.run("bad", () => { throw new Error("service failed"); });
  });
  const first = await execute().catch(error => error);
  const second = await execute().catch(error => error);
  expect(second.code).toBe(first.code);
  expect(second.message).toBe(first.message);
  expect(second.failure).toEqual(first.failure);
});

test("a storage failure wins over an earlier usage refusal without losing its identity", async () => {
  const flushed = new Error("completion flush failed");
  let bytes = Buffer.alloc(0);
  const storage: JournalStorage = {
    async readEvents() { return bytes; },
    async append(record) {
      if (JSON.parse(Buffer.from(record).toString()).type === "step.completed") throw flushed;
      bytes = Buffer.concat([bytes, Buffer.from(record)]);
    },
    async truncateEvents(size) { bytes = bytes.subarray(0, size); },
    async replaceSnapshot() {}, async close() {},
  };
  const journal = await RunJournal.recover(storage, { now: () => Date.parse("2026-01-01T00:00:00Z"), schedule: () => () => {} });
  journals.push(journal);
  const started = Promise.withResolvers<void>();
  const finish = Promise.withResolvers<null>();
  const execution = runWorkflow(journal, identity, async (_, { step }) => {
    const first = step.run("first", () => { started.resolve(); return finish.promise; });
    await started.promise;
    await step.run("overlap", () => null).catch(() => {});
    finish.resolve(null);
    return await first;
  });
  await expect(execution).rejects.toBe(flushed);
  expect(journal.state.status).toBe("running");
});
