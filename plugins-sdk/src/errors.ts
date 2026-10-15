import { z } from "zod";

export type ErrorCode = "VALIDATION" | "NOT_FOUND" | "FORBIDDEN" | "INTERNAL";

/** What a failed call returns: a code, a sentence, and per-field messages keyed by JSON pointer ("/minutes"). */
export type CallError = { code: ErrorCode; message: string; fieldErrors?: Record<string, string> };

export class MethodError extends Error {
  constructor(
    readonly code: ErrorCode,
    message: string,
    readonly fieldErrors?: Record<string, string>,
  ) {
    super(message);
  }
}

export function toCallError(error: unknown): CallError {
  if (error instanceof MethodError) {
    return { code: error.code, message: error.message, ...(error.fieldErrors ? { fieldErrors: error.fieldErrors } : {}) };
  }
  if (error instanceof z.ZodError) {
    const fieldErrors: Record<string, string> = {};
    for (const issue of error.issues) fieldErrors["/" + issue.path.join("/")] ??= issue.message;
    return { code: "VALIDATION", message: "Check the highlighted fields.", fieldErrors };
  }
  return { code: "INTERNAL", message: error instanceof Error ? error.message : String(error) };
}
