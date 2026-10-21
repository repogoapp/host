import type { JournalFailure } from "./events";

export class NonRetryable extends Error {
  constructor(message: string) { super(message); this.name = "NonRetryable"; }
}

export class RunnerError extends Error {
  constructor(
    readonly code: "FAILED" | "BUSY" | "IDENTITY" | "REPLAY" | "CHANGED" | "ATTENTION" | "STEP" | "UNKNOWN" | "CANCELLED" | "EXPIRED" | "PAUSED" | "USAGE" | "WAITING",
    message: string,
    readonly failure: JournalFailure | null = null,
  ) { super(message); this.name = "RunnerError"; }
}

export class RunWaiting extends RunnerError {
  constructor(readonly stepId: string, readonly attempt: number, readonly nextAttemptAt: string) {
    super("WAITING", `Step ${stepId} is waiting until ${nextAttemptAt}.`);
  }
}

export function failure(error: unknown): JournalFailure {
  if (error instanceof RunnerError && error.failure) return structuredClone(error.failure);
  return { message: (error instanceof Error ? error.message : String(error)).slice(0, 4000), ...(error instanceof RunnerError ? { code: error.code } : {}) };
}

export function asError(error: unknown): Error {
  return error instanceof Error ? error : new Error(String(error));
}

export function outcomeError(status: "failed" | "unknown", error: JournalFailure): RunnerError {
  return new RunnerError(status === "unknown" ? "UNKNOWN" : "FAILED", error.message, error);
}

export function terminalError(status: "cancelled" | "expired"): RunnerError {
  return new RunnerError(status === "cancelled" ? "CANCELLED" : "EXPIRED", status === "cancelled" ? "Workflow cancelled." : "Workflow deadline reached.");
}
