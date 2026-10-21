import type { describe } from "./summary";
import { z } from "zod";
import { isAbsolute } from "node:path";
import { DurationMsSchema } from "./policy";
import type { HostCaller, SentNotice } from "../runtime";
import type { LogRecord } from "../logs";

export const RunSummarySchema = z.object({
  id: z.string(), workflow: z.string(), status: z.enum(["pending", "running", "waiting", "attention", "completed", "failed", "cancelled", "expired", "unknown", "blocked"]), currentStep: z.string().nullable(),
  attempt: z.number().int(), createdAt: z.string(), updatedAt: z.string(),
  nextAttemptAt: z.string().nullable(), deadlineAt: z.string().nullable(),
});
export type RunSummary = z.infer<typeof RunSummarySchema>;
export type WorkflowTarget = { dir: string; plugin: string; namespace: string; workflow: string; root: string };
export type WorkflowRequest = WorkflowTarget & {
  operation: "start" | "list" | "get" | "cancel" | "cancelAll" | "retry" | "restart";
  input: Record<string, unknown>;
  caller?: "card" | "agent" | "cron" | "code" | "http";
  settings?: { timeoutMs: number; maxOfflineMs: number; retentionMs: number };
};
export type WorkflowBridge = (request: WorkflowRequest) => Promise<unknown>;
export const RunMetadataSchema = z.object({
  id: z.string().uuid(),
  dir: z.string().refine(isAbsolute), root: z.string().refine(isAbsolute),
  namespace: z.string().regex(/^[a-z0-9][a-z0-9.-]*$/), plugin: z.string().regex(/^[a-z0-9][a-z0-9-]*$/),
  caller: z.enum(["card", "agent", "cron", "code", "http"]),
  workflow: z.string().regex(/^[a-z][a-zA-Z0-9_-]*$/), input: z.json(), key: z.string().nullable(),
  timeoutMs: DurationMsSchema, maxOfflineMs: DurationMsSchema, retentionMs: DurationMsSchema, createdAt: z.iso.datetime(),
}).strict();
export const RunLocationSchema = RunMetadataSchema.omit({ input: true, timeoutMs: true, workflow: true, createdAt: true });
export type RunMetadata = z.infer<typeof RunMetadataSchema>;
export type ExecutorStart = { type: "start"; run: RunMetadata; directory: string; dataDir: string };
export type ExecutorMessage =
  | { type: "host"; id: number; plugin: string; method: string; params: unknown }
  | { type: "state"; summary: RunSummary; detail: ReturnType<typeof describe> }
  | { type: "invalidated"; tags: string[] }
  | { type: "notice"; notice: SentNotice }
  | { type: "log"; records: LogRecord[] }
  | { type: "done" }
  | { type: "error"; message: string };
export type WorkflowCallbacks = {
  host: HostCaller;
  invalidate: (tags: string[]) => void;
  notify: (notice: SentNotice) => void;
  logs: (dir: string, records: LogRecord[]) => void;
};
