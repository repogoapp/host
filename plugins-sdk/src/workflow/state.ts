import { isDeepStrictEqual } from "node:util";
import type { RunMetadata } from "./contracts";
import { deadlineFrom } from "./clock";
import type { StepPolicy } from "./policy";
import { JournalError, sleepId, type JournalEvent, type JournalFailure, type JSONValue } from "./events";

export type StepState = {
  status: "running" | "waiting" | "completed" | "failed" | "unknown";
  attempt: number;
  limit: number;
  policy: StepPolicy;
  result: JSONValue;
  error: JournalFailure | null;
  nextAttemptAt: string | null;
};

export type SleepState = { status: "waiting" | "completed"; durationMs: number; wakeAt: string };

export type RunState = {
  version: 2;
  lastSeq: number;
  status: "pending" | "running" | "waiting" | "attention" | "completed" | "failed" | "cancelled" | "expired" | "unknown";
  workflow: string | null;
  input: JSONValue;
  currentStep: string | null;
  operationOrder: string[];
  steps: Record<string, StepState>;
  sleeps: Record<string, SleepState>;
  result: JSONValue;
  error: JournalFailure | null;
  expiryReason: string | null;
  deadlineAt: string | null;
  /** The run's lifetime, which a restart starts again. */
  timeoutMs: number | null;
  cancelRequested: boolean;
  updatedAt: string | null;
  endedAt: string | null;
  metadata: RunMetadata | null;
};

export function initialState(): RunState {
  return {
    version: 2, lastSeq: 0, status: "pending", workflow: null, input: null,
    currentStep: null, operationOrder: [], steps: {}, sleeps: {}, result: null, error: null, expiryReason: null, deadlineAt: null, timeoutMs: null, cancelRequested: false, updatedAt: null, endedAt: null, metadata: null,
  };
}

/** One reducer defines state both during execution and journal recovery. */
export function applyEvent(state: RunState, event: JournalEvent): RunState {
  if (event.seq !== state.lastSeq + 1) refuse("Event sequence is not contiguous.");
  const next = { ...state, lastSeq: event.seq, updatedAt: event.at };
  if (event.type === "run.started") {
    if (state.status !== "pending") refuse("The run already started.");
    return { ...next, metadata: event.location ? { ...event.location, workflow: event.workflow, input: event.input, timeoutMs: event.timeoutMs, createdAt: event.at } : null, status: "running", workflow: event.workflow, input: event.input, timeoutMs: event.timeoutMs, deadlineAt: deadlineFrom(event.at, event.timeoutMs) };
  }
  if (event.type === "run.retryRequested") {
    const step = Object.hasOwn(state.steps, event.stepId) ? state.steps[event.stepId]! : null;
    if (state.status !== "failed" || state.cancelRequested || !step || step.status !== "failed" || step.policy.kind !== "query" ||
      !["STEP", "TIMEOUT", "INTERRUPTED"].includes(step.error?.code ?? "") ||
      Object.values(state.sleeps).some(sleep => sleep.status !== "completed") ||
      Object.entries(state.steps).some(([id, value]) => id !== event.stepId && value.status !== "completed")) refuse("Only a safely failed query step can be retried.");
    if (state.deadlineAt && Date.parse(event.at) >= Date.parse(state.deadlineAt)) refuse("The workflow lifetime has ended.");
    const limit = step!.attempt + step!.policy.attempts;
    if (limit > 10000) refuse("The run's attempt limit is exhausted.");
    return { ...next, endedAt: null, status: "waiting", error: null, currentStep: event.stepId, steps: { ...state.steps,
      [event.stepId]: { ...step!, status: "waiting", limit, nextAttemptAt: event.at },
    } };
  }
  if (event.type === "run.restarted") {
    if (state.status !== "attention") refuse("Only a run that needs attention can restart.");
    // Progress and the run's lifetime start over, on the current code.
    const deadlineAt = state.timeoutMs === null ? state.deadlineAt : deadlineFrom(event.at, state.timeoutMs);
    return { ...next, status: "running", error: null, currentStep: null, operationOrder: [], steps: {}, sleeps: {}, deadlineAt };
  }
  if (state.status === "attention") {
    if (event.type === "run.cancelRequested") return { ...next, cancelRequested: true };
    if (event.type === "run.cancelled") return { ...next, endedAt: event.at, status: "cancelled" };
    if (event.type === "run.expired") return { ...next, endedAt: event.at, status: "expired", expiryReason: event.reason };
    refuse("The run needs attention: restart or cancel it.");
  }
  if (state.status !== "running" && state.status !== "waiting") refuse("The run is not active.");
  if (event.type === "run.attention") {
    if (Object.values(state.steps).some(step => step.status === "running")) refuse("A step is running.");
    return { ...next, status: "attention", error: event.error };
  }
  if (event.type === "run.cancelRequested") return { ...next, cancelRequested: true };
  if (event.type === "run.completed") {
    if (state.cancelRequested) refuse("Cancellation prevents run completion.");
    if (Object.values(state.sleeps).some(sleep => sleep.status !== "completed") || Object.values(state.steps).some(step => step.status !== "completed")) refuse("An unfinished or failed step prevents run completion.");
    return { ...next, endedAt: event.at, status: "completed", result: event.result };
  }
  if (event.type === "run.failed" || event.type === "run.unknown") {
    return { ...next, endedAt: event.at, status: event.type === "run.failed" ? "failed" : "unknown", error: event.error };
  }
  if (event.type === "run.cancelled") return { ...next, endedAt: event.at, status: "cancelled" };
  if (event.type === "run.expired") return { ...next, endedAt: event.at, status: "expired", expiryReason: event.reason };

  if (event.type === "sleep.scheduled") {
    if (state.cancelRequested) refuse("Cancellation prevents starting sleeps.");
    if (event.sleepId !== sleepId(state.operationOrder.length + 1) || Object.hasOwn(state.sleeps, event.sleepId)) refuse("Sleep identity must match its operation position.");
    if (Object.values(state.steps).some(step => step.status !== "completed") || Object.values(state.sleeps).some(sleep => sleep.status !== "completed")) refuse("Another operation is unfinished.");
    const sleep: SleepState = { status: "waiting", durationMs: event.durationMs, wakeAt: deadlineFrom(event.at, event.durationMs) };
    return { ...next, status: "waiting", currentStep: event.sleepId, operationOrder: [...state.operationOrder, event.sleepId], sleeps: { ...state.sleeps, [event.sleepId]: sleep } };
  }
  if (event.type === "sleep.completed") {
    const sleep = Object.hasOwn(state.sleeps, event.sleepId) ? state.sleeps[event.sleepId] : null;
    if (!sleep || sleep.status !== "waiting") refuse("Only a waiting sleep can complete.");
    if (state.cancelRequested) refuse("Cancellation prevents completing sleeps.");
    if (Date.parse(event.at) < Date.parse(sleep!.wakeAt)) throw new JournalError("NOT_DUE", "The sleep is not due.");
    return { ...next, status: "running", currentStep: event.sleepId, sleeps: { ...state.sleeps, [event.sleepId]: { ...sleep!, status: "completed" } } };
  }

  const previous = Object.hasOwn(state.steps, event.stepId) ? state.steps[event.stepId] : undefined;
  let step: StepState;
  if (event.type === "step.started") {
    if (state.cancelRequested) refuse("Cancellation prevents starting steps.");
    if (Object.values(state.sleeps).some(sleep => sleep.status !== "completed")) refuse("A sleep is unfinished.");
    if (Object.values(state.steps).some(step => step.status === "failed")) refuse("A failed step prevents starting steps.");
    if (previous) {
      if (!isDeepStrictEqual(previous.policy, event.policy)) refuse("Retry policy differs from its recorded policy.");
      if (previous.status !== "waiting" || event.attempt !== previous.attempt + 1) refuse("Only a scheduled retry can start the next attempt.");
      if (Date.parse(event.at) < Date.parse(previous.nextAttemptAt!)) throw new JournalError("NOT_DUE", "The retry is not due.");
    } else if (event.attempt !== 1) {
      refuse("A new step starts at attempt one.");
    }
    if (event.attempt > (previous?.limit ?? event.policy.attempts)) refuse("The attempt budget is exhausted.");
    if (Object.entries(state.steps).some(([id, value]) => id !== event.stepId && active(value))) refuse("Another step is unfinished.");
    step = { status: "running", attempt: event.attempt, limit: previous?.limit ?? event.policy.attempts, result: null, error: null, nextAttemptAt: null, policy: event.policy };
  } else {
    if (!previous || previous.status !== "running" || previous.attempt !== event.attempt) refuse("The event does not match a running attempt.");
    if (event.type === "step.completed") {
      step = { ...previous, status: "completed", result: event.result, error: null, nextAttemptAt: null };
    } else if (event.type === "step.failed" || event.type === "step.unknown") {
      step = { ...previous, status: event.type === "step.failed" ? "failed" : "unknown", error: event.error };
    } else {
      if (previous.policy.kind !== "query" || previous.attempt >= previous.limit) refuse("The step cannot retry.");
      if (state.cancelRequested) refuse("Cancellation prevents retries.");
      const nextAttemptAt = deadlineFrom(event.at, event.delayMs);
      step = { ...previous, status: "waiting", error: event.error, nextAttemptAt };
    }
  }
  return {
    ...next, status: step.status === "waiting" ? "waiting" : "running", currentStep: event.stepId,
    operationOrder: previous ? state.operationOrder : [...state.operationOrder, event.stepId],
    steps: { ...state.steps, [event.stepId]: step },
  };
}

function active(step: StepState): boolean {
  return step.status === "running" || step.status === "waiting" || step.status === "unknown";
}

function refuse(message: string): never {
  throw new JournalError("INVALID_EVENT", message);
}
