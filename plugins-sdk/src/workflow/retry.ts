import { isDeepStrictEqual } from "node:util";
import { invokeAttempt, type StepContext } from "./attempts";
import { deadlineFrom, type WorkflowClock } from "./clock";
import { RunWaiting, RunnerError, failure } from "./errors";
import { eventInput, JournalError, type JournalEvent, type JournalFailure, type JSONValue } from "./events";
import type { RunJournal } from "./journal";
import type { RunLifetime } from "./lifetime";
import { retryDelay, type StepPolicy } from "./policy";
import type { StepState } from "./state";

export type ExecutionEnvironment = { clock: WorkflowClock; random: () => number };

export async function scheduleRetry(
  append: RunJournal["append"], name: string, attempt: number, policy: StepPolicy,
  error: JournalFailure, environment: ExecutionEnvironment,
): Promise<RunWaiting> {
  const delayMs = retryDelay(policy, attempt, environment.random);
  const event = await append({ type: "step.retryScheduled", stepId: name, attempt, error, delayMs });
  const nextAttemptAt = deadlineFrom(event.at, delayMs);
  return new RunWaiting(name, attempt, nextAttemptAt);
}

export async function executeStep<T extends JSONValue>(
  name: string, callback: (ctx: StepContext) => T | Promise<T>, policy: StepPolicy, saved: StepState | undefined,
  append: RunJournal["append"], environment: ExecutionEnvironment, lifetime: RunLifetime,
  stopped: () => unknown, markUncertain: () => void,
): Promise<T> {
  if (saved && !isDeepStrictEqual(saved.policy, policy)) throw new RunnerError("REPLAY", `Step ${name} policy changed.`);
  if (saved?.status === "completed") return structuredClone(saved.result) as T;
  if (saved && saved.status !== "waiting") throw new RunnerError("REPLAY", `Step ${name} is not resumable.`);
  if (saved?.nextAttemptAt && environment.clock.now() < Date.parse(saved.nextAttemptAt)) {
    throw new RunWaiting(name, saved.attempt, saved.nextAttemptAt);
  }
  const limit = saved?.limit ?? policy.attempts;
  const attempt = saved ? saved.attempt + 1 : 1;
  try { await append({ type: "step.started", stepId: name, attempt, policy }); }
  catch (error) {
    if (error instanceof JournalError && error.code === "NOT_DUE" && saved?.nextAttemptAt) {
      throw new RunWaiting(name, saved.attempt, saved.nextAttemptAt);
    }
    throw error;
  }
  lifetime.check();
  if (lifetime.reason === "paused") throw new RunnerError("PAUSED", "Workflow executor suspended.");
  if (lifetime.reason) {
    const error = { code: lifetime.reason === "cancelled" ? "CANCELLED" : "EXPIRED", message: "Workflow stopped before dispatch." };
    await append({ type: "step.failed", stepId: name, attempt, error });
    throw new RunnerError("STEP", error.message, error);
  }
  if (stopped()) {
    const error = failure(stopped());
    await append({ type: "step.failed", stepId: name, attempt, error });
    throw new RunnerError("STEP", error.message, error);
  }
  const outcome = await invokeAttempt(callback, name, attempt, policy, limit, environment.clock, lifetime.controller.signal);
  lifetime.check();
  if (outcome.type === "stopped" || lifetime.reason) {
    if (outcome.type === "stopped" && !outcome.invoked && lifetime.reason !== "paused") {
      const error = { code: lifetime.reason === "expired" ? "EXPIRED" : "CANCELLED", message: "Workflow stopped before dispatch." };
      await append({ type: "step.failed", stepId: name, attempt, error });
    }
    throw new RunnerError("USAGE", "The workflow stopped during its step.");
  }
  if (outcome.type === "unknown") {
    await append({ type: "step.unknown", stepId: name, attempt, error: outcome.error });
    markUncertain();
    throw new RunnerError("UNKNOWN", outcome.error.message, outcome.error);
  }
  if (outcome.type === "failed") {
    if (!stopped() && outcome.retryable && attempt < limit) {
      throw await scheduleRetry(append, name, attempt, policy, outcome.error, environment);
    }
    await append({ type: "step.failed", stepId: name, attempt, error: outcome.error });
    throw new RunnerError("STEP", outcome.error.message, outcome.error);
  }
  const invalidResult = async (message: string): Promise<never> => {
    const error = { code: "INVALID_RESULT", message };
    const uncertain = policy.kind === "mutation";
    await append({ type: uncertain ? "step.unknown" : "step.failed", stepId: name, attempt, error });
    if (uncertain) markUncertain();
    throw new RunnerError(uncertain ? "UNKNOWN" : "STEP", error.message, error);
  };
  let completion;
  try { completion = eventInput({ type: "step.completed", stepId: name, attempt, result: outcome.result }); }
  catch { return await invalidResult(`Step ${name} returned a result that cannot be persisted as JSON.`); }
  let committed: JournalEvent;
  try { committed = await append(completion); }
  catch (error) {
    if (error instanceof JournalError && error.code === "LIMIT") return await invalidResult(`Step ${name} result exceeds the journal limit.`);
    throw error;
  }
  return (committed as Extract<JournalEvent, { type: "step.completed" }>).result as T;
}
