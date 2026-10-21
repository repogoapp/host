import type { RunMetadata, RunSummary } from "./contracts";
import { initialState, type RunState } from "./state";
import type { JournalFailure, JSONValue } from "./events";

export function summarize(run: RunMetadata, state: RunState): RunSummary {
  const step = state.currentStep && Object.hasOwn(state.steps, state.currentStep) ? state.steps[state.currentStep] : null;
  const sleep = state.currentStep && Object.hasOwn(state.sleeps, state.currentStep) ? state.sleeps[state.currentStep] : null;
  return {
    id: run.id, workflow: run.workflow, status: state.status, currentStep: state.currentStep,
    attempt: step?.attempt ?? 0, createdAt: run.createdAt,
    updatedAt: state.updatedAt ?? run.createdAt, nextAttemptAt: !activeStatuses.has(state.status) ? null : sleep?.status === "waiting" ? sleep.wakeAt : step?.nextAttemptAt ?? null,
    deadlineAt: state.deadlineAt,
  };
}

export const terminalStatuses = new Set(["completed", "failed", "cancelled", "expired", "unknown"]);
export const activeStatuses = new Set(["pending", "running", "waiting"]);
/** Not finished: what Cancel all reaches, and what a deadline can still expire. */
export const openStatuses = new Set([...activeStatuses, "attention"]);

export function describe(state: RunState) {
  const bounded = (value: unknown) => JSON.stringify(value).length <= 16 * 1024 ? value : null;
  const result = bounded(state.result);
  return {
    error: briefError(state.error), input: bounded(state.input), result,
    resultTruncated: result === null && state.result !== null,
    inputTruncated: bounded(state.input) === null && state.input !== null,
    expiryReason: state.expiryReason, cancelRequested: state.cancelRequested,
    steps: state.operationOrder.slice(-20).map(id => {
      const sleep = Object.hasOwn(state.sleeps, id) ? state.sleeps[id] : null;
      if (sleep) return { id, kind: "sleep", status: sleep.status, attempt: 0, attempts: 0, error: null, nextAttemptAt: activeStatuses.has(state.status) && sleep.status === "waiting" ? sleep.wakeAt : null, durationMs: sleep.durationMs };
      const step = state.steps[id]!;
      return { id, kind: "step", durationMs: null, status: step.status, attempt: step.attempt, attempts: step.limit, error: briefError(step.error), nextAttemptAt: activeStatuses.has(state.status) ? step.nextAttemptAt : null };
    }),
  };
}

export function briefError(error: JournalFailure | null) {
  return error ? { ...error, message: error.message.slice(0, 4000) } : null;
}
export function unavailableDetail(input: JSONValue, error: string | null) {
  return describe({ ...initialState(), input, error: error ? { message: error } : null });
}
