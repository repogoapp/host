import { attemptScope } from "./scope";
import { currentAccess, tracked } from "../tracking";
import type { WorkflowClock } from "./clock";
import { failure, NonRetryable } from "./errors";
import type { JournalFailure, JSONValue } from "./events";
import type { StepPolicy } from "./policy";

export type StepContext = { stepId: string; attempt: number; attempts: number; signal: AbortSignal };
export type AttemptOutcome<T> =
  | { type: "completed"; result: T }
  | { type: "failed"; error: JournalFailure; retryable: boolean }
  | { type: "unknown"; error: JournalFailure }
  | { type: "stopped"; invoked: boolean };

/** A timed-out invocation retains ownership until its callback settles. */
export async function invokeAttempt<T extends JSONValue>(
  callback: (ctx: StepContext) => T | Promise<T>,
  stepId: string, attempt: number, policy: StepPolicy, limit: number,
  clock: WorkflowClock, signal: AbortSignal,
): Promise<AttemptOutcome<T>> {
  const controller = new AbortController();
  const deadlineAt = clock.now() + policy.timeoutMs;
  let invoked = false;
  let timedOut = false;
  const abort = () => controller.abort(signal.reason);
  const timeout = () => {
    timedOut = true;
    controller.abort(new Error("Step attempt timed out."));
  };
  signal.addEventListener("abort", abort, { once: true });
  const removeTimer = clock.schedule(deadlineAt, timeout);
  try {
    if (signal.aborted) return { type: "stopped", invoked };
    if (clock.now() >= deadlineAt) timeout();
    let result: T | undefined;
    let caught: { error: unknown } | null = null;
    if (!timedOut) {
      invoked = true;
      try {
        const context = { stepId, attempt, attempts: limit, signal: controller.signal };
        if (policy.kind === "query") {
          const execution = await tracked(true, () => {}, async () => await attemptScope.run(context, () => callback(context)));
          if (!execution.ok) throw execution.error;
          result = execution.value;
        } else result = await attemptScope.run(context, () => callback(context));
      }
      catch (error) { caught = { error }; }
    }
    if (signal.aborted && (!invoked || caught)) return { type: "stopped", invoked };
    if (clock.now() >= deadlineAt) timedOut = true;
    if (timedOut) {
      const error = { code: "TIMEOUT", message: `Step ${stepId} attempt ${attempt} timed out.` };
      if (!invoked) return { type: "failed", error, retryable: false };
      return policy.kind === "query" ? { type: "failed", error, retryable: true } : { type: "unknown", error };
    }
    if (caught) return {
      type: "failed", error: { message: failure(caught.error).message, code: caught.error instanceof NonRetryable ? "NON_RETRYABLE" : "STEP" },
      retryable: policy.kind === "query" && !(caught.error instanceof NonRetryable),
    };
    return { type: "completed", result: result as T };
  } finally {
    const access = currentAccess();
    if (access?.writes.size) { access.announce([...access.writes]); access.writes.clear(); }
    removeTimer();
    signal.removeEventListener("abort", abort);
  }
}
