import { isDeepStrictEqual } from "node:util";
import type { Duration } from "../duration";
import type { StepContext } from "./attempts";
import { RunWaiting, RunnerError, asError, failure, outcomeError, terminalError } from "./errors";
import { eventInput, sleepId, JournalError, type JournalEvent, type JSONValue } from "./events";
import { RunJournal } from "./journal";
import { settleInterrupted } from "./interrupted";
import { attemptScope } from "./scope";
import { executeSleep } from "./sleep";
import { RunLifetime } from "./lifetime";
import { durationMs, stepPolicy, type StepOptions } from "./policy";
import { executeStep, scheduleRetry, type ExecutionEnvironment } from "./retry";

export { RunnerError, RunWaiting, NonRetryable } from "./errors";
export type { StepContext } from "./attempts";
export type { StepOptions } from "./policy";
export type RunIdentity = { workflow: string; input: JSONValue };
export type WorkflowSteps = {
  run<T extends JSONValue>(name: string, callback: (ctx: StepContext) => T | Promise<T>, options?: StepOptions): Promise<T>;
};
export type RunContext = { step: WorkflowSteps; sleep: (duration: Duration) => Promise<void>; signal: AbortSignal };
export type RunOptions = { timeout?: Duration; maxOffline?: Duration; signal?: AbortSignal; suspend?: AbortSignal; random?: () => number };

const owners = new WeakSet<RunJournal>();

/** Internal executor over the current code; callers own journal closure. */
export async function runWorkflow<T extends JSONValue>(
  journal: RunJournal, identity: RunIdentity,
  handler: (input: JSONValue, ctx: RunContext) => T | Promise<T>, options: RunOptions = {},
): Promise<T> {
  if (owners.has(journal)) throw new RunnerError("BUSY", "This journal already has an executor.");
  owners.add(journal);
  let lifetime: RunLifetime | null = null;
  try {
    const environment = { clock: journal.clock, random: options.random ?? Math.random };
    const timeoutMs = durationMs(options.timeout ?? "30d");
    const start = eventInput({ type: "run.started", ...identity, timeoutMs });
    if (start.type !== "run.started") throw new RunnerError("IDENTITY", "Invalid run identity.");
    let state = journal.state;
    if (state.status === "pending") {
      await journal.append(start);
      state = journal.state;
    } else if (state.workflow !== start.workflow || !isDeepStrictEqual(state.input, start.input)) {
      throw new RunnerError("IDENTITY", "Workflow and input must match the recorded run.");
    }
    if (state.status === "attention") throw new RunnerError("ATTENTION", state.error!.message, state.error);
    if (state.status === "completed") return structuredClone(state.result) as T;
    if (state.status === "failed" || state.status === "unknown") throw outcomeError(state.status, state.error!);
    if (state.status === "cancelled" || state.status === "expired") throw terminalError(state.status);
    if (state.status !== "running" && state.status !== "waiting") throw new RunnerError("USAGE", "The run is not active.");
    if (state.cancelRequested) {
      await settleInterrupted(journal, "CANCELLED");
      await journal.append({ type: "run.cancelled" });
      throw terminalError("cancelled");
    }
    lifetime = new RunLifetime(journal, environment.clock, state.deadlineAt, options.signal, options.suspend, options.maxOffline === undefined ? undefined : durationMs(options.maxOffline));
    if (lifetime.reason) await settleControl(journal, lifetime);
    const unknown = Object.values(state.steps).find(step => step.status === "unknown");
    if (unknown) {
      await settleControl(journal, lifetime);
      await journal.append({ type: "run.unknown", error: unknown.error! });
      throw outcomeError("unknown", unknown.error!);
    }
    const interrupted = Object.entries(state.steps).find(([, step]) => step.status === "running");
    if (interrupted) {
      await settleControl(journal, lifetime);
      const [name, step] = interrupted;
      const error = { code: "INTERRUPTED", message: `Step ${name} attempt ${step.attempt} was interrupted.` };
      if (step.policy.kind === "query") {
        if (step.attempt < step.limit) {
          const waiting = await scheduleRetry(journal.append.bind(journal), name, step.attempt, step.policy, error, environment);
          await settleControl(journal, lifetime);
          throw waiting;
        }
        await journal.append({ type: "step.failed", stepId: name, attempt: step.attempt, error });
        await settleControl(journal, lifetime);
        await journal.append({ type: "run.failed", error });
        throw outcomeError("failed", error);
      }
      await settleControl(journal, lifetime);
      await journal.append({ type: "run.unknown", error });
      throw outcomeError("unknown", error);
    }
    const failed = Object.values(state.steps).find(step => step.status === "failed");
    if (failed) {
      await settleControl(journal, lifetime);
      await journal.append({ type: "run.failed", error: failed.error! });
      throw outcomeError("failed", failed.error!);
    }
    const waiting = Object.entries(state.steps).find(([, step]) => step.status === "waiting");
    if (waiting && environment.clock.now() < Date.parse(waiting[1].nextAttemptAt!)) {
      await settleControl(journal, lifetime);
      throw new RunWaiting(waiting[0], waiting[1].attempt, waiting[1].nextAttemptAt!);
    }
    const sleeping = Object.entries(state.sleeps).find(([, sleep]) => sleep.status === "waiting");
    if (sleeping && environment.clock.now() < Date.parse(sleeping[1].wakeAt)) {
      await settleControl(journal, lifetime);
      throw new RunWaiting(sleeping[0], 0, sleeping[1].wakeAt);
    }
    return await execute(journal, handler, environment, lifetime);
  } finally {
    try { await lifetime?.close(); } finally { owners.delete(journal); }
  }
}

async function execute<T extends JSONValue>(
  journal: RunJournal, handler: (input: JSONValue, ctx: RunContext) => T | Promise<T>,
  environment: ExecutionEnvironment, lifetime: RunLifetime,
): Promise<T> {
  const initial = journal.state;
  const history = initial.operationOrder;
  const seen = new Set<string>();
  let cursor = 0;
  let accepting = true;
  const flight: { current: Promise<unknown> | null } = { current: null };
  let stopped: unknown = null;
  let storageFailed = false;
  let storageError: unknown;
  let uncertain = false;
  const append: RunJournal["append"] = async event => {
    try { return await journal.append(event); }
    catch (error) {
      if (!(error instanceof JournalError && ["LIMIT", "NOT_DUE", "INVALID_EVENT"].includes(error.code))) {
        if (!storageFailed) storageError = error;
        storageFailed = true;
        stopped ??= asError(error);
      }
      throw error;
    }
  };
  function claim(name: string) {
    if (seen.has(name)) throw new RunnerError("REPLAY", `Operation ${name} was called more than once.`);
    if (cursor < history.length && history[cursor] !== name) throw changed(`expected ${history[cursor]}, but the code ran ${name}`);
    seen.add(name);
    cursor += 1;
  }
  function check() {
    lifetime.check();
    if (lifetime.reason) throw new RunnerError("USAGE", "The workflow stopped during its operation.");
    if (stopped) throw stopped;
  }
  function invoke<TValue>(perform: () => Promise<TValue>): Promise<TValue> {
    lifetime.check();
    let refusal = stopped;
    if (attemptScope.getStore()) refusal = new RunnerError("USAGE", "Workflow operations cannot run inside a step callback.");
    else if (!accepting || flight.current || lifetime.reason) refusal = new RunnerError("USAGE", "Workflow operations must be awaited sequentially within an active run.");
    if (refusal) {
      stopped ??= asError(refusal);
      const rejected = Promise.reject<TValue>(refusal);
      void rejected.catch(() => {});
      return rejected;
    }
    const invocation = (async () => {
      try { return await perform(); }
      catch (error) { stopped ??= asError(error); throw error; }
      finally { flight.current = null; }
    })();
    flight.current = invocation;
    void invocation.catch(() => {});
    return invocation;
  }
  const step: WorkflowSteps = {
    run: (name, callback, options) => invoke(async () => {
      let policy;
      try { policy = stepPolicy(options); }
      catch (error) { throw new RunnerError("USAGE", failure(error).message); }
      try { eventInput({ type: "step.started", stepId: name, attempt: 1, policy }); }
      catch { throw new RunnerError("USAGE", "Step names must be 1–200 characters and may not start with @sleep:."); }
      claim(name);
      const saved = Object.hasOwn(initial.steps, name) ? initial.steps[name] : undefined;
      return executeStep(name, callback, policy, saved, append, environment, lifetime, () => stopped, () => { uncertain = true; });
    }),
  };
  const sleep: RunContext["sleep"] = duration => invoke(async () => {
    const id = sleepId(cursor + 1);
    claim(id);
    return executeSleep(id, duration, initial.sleeps[id], append, environment.clock, check);
  });
  let result: T | undefined;
  try { result = await handler(initial.input, { step, sleep, signal: lifetime.controller.signal }); }
  catch (error) { stopped ??= asError(error); }
  accepting = false;
  if (flight.current) {
    stopped ??= new RunnerError("USAGE", "The workflow returned before awaiting its step.");
    await flight.current.catch(() => {});
  }
  await lifetime.close();
  if (storageFailed) throw storageError;
  await settleControl(journal, lifetime);
  if (!stopped && cursor < history.length) stopped = changed(`${history[cursor]} was recorded, but the code no longer runs it`);
  if (stopped instanceof RunWaiting) throw stopped;
  if (stopped instanceof RunnerError && stopped.code === "CHANGED" && !uncertain) {
    const recorded = failure(stopped);
    await append({ type: "run.attention", error: recorded });
    throw new RunnerError("ATTENTION", recorded.message, recorded);
  }
  if (stopped) {
    const recorded = failure(stopped);
    await append({ type: uncertain ? "run.unknown" : "run.failed", error: recorded });
    throw outcomeError(uncertain ? "unknown" : "failed", recorded);
  }
  let completion;
  try { completion = eventInput({ type: "run.completed", result }); }
  catch {
    const error = new RunnerError("USAGE", "The workflow result must be JSON.");
    await append({ type: "run.failed", error: failure(error) });
    throw outcomeError("failed", failure(error));
  }
  try {
    const committed = await append(completion) as Extract<JournalEvent, { type: "run.completed" }>;
    return committed.result as T;
  } catch (error) {
    if (error instanceof JournalError && error.code === "LIMIT") {
      const recorded = failure(error);
      await append({ type: "run.failed", error: recorded });
      throw outcomeError("failed", recorded);
    }
    throw error;
  }
}

/** The code no longer matches the run's recorded steps: an edit, not a bug, so the run waits for the user. */
function changed(detail: string) {
  return new RunnerError("CHANGED", `The workflow changed since this run started: ${detail}. Restart the run or cancel it.`);
}

async function settleControl(journal: RunJournal, lifetime: RunLifetime): Promise<void> {
  await lifetime.close();
  if (lifetime.reason === "paused") throw new RunnerError("PAUSED", "Workflow executor suspended.");
  if (lifetime.reason === "cancelled") {
    await settleInterrupted(journal, "CANCELLED");
    await journal.append({ type: "run.cancelled" });
    throw terminalError("cancelled");
  }
  if (lifetime.reason === "expired") {
    await settleInterrupted(journal, "EXPIRED");
    await journal.append({ type: "run.expired", reason: lifetime.expiryReason });
    throw terminalError("expired");
  }
}
