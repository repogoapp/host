import { z } from "zod";
import { RunLocationSchema } from "./contracts";
import { DurationMsSchema, maxDurationMs, StepPolicySchema } from "./policy";

const ID = z.string().min(1).max(200);
export const sleepId = (position: number) => `@sleep:${position}`;
const SleepID = z.string().regex(/^@sleep:[1-9]\d*$/);
const StepID = ID.refine(value => !value.startsWith("@sleep:"), "Sleep IDs are reserved for the workflow core.");
const Attempt = z.number().int().positive();
const Failure = z.object({ message: z.string(), code: z.string().optional() }).strict();
const Step = { stepId: StepID, attempt: Attempt };

export const EventInputSchema = z.discriminatedUnion("type", [
  z.object({ type: z.literal("run.started"), workflow: ID, input: z.json(), timeoutMs: DurationMsSchema, location: RunLocationSchema.optional() }).strict(),
  z.object({ type: z.literal("sleep.scheduled"), sleepId: SleepID, durationMs: DurationMsSchema }).strict(),
  z.object({ type: z.literal("sleep.completed"), sleepId: SleepID }).strict(),
  z.object({ type: z.literal("step.started"), ...Step, policy: StepPolicySchema }).strict(),
  z.object({ type: z.literal("step.completed"), ...Step, result: z.json() }).strict(),
  z.object({ type: z.literal("step.failed"), ...Step, error: Failure }).strict(),
  z.object({ type: z.literal("step.unknown"), ...Step, error: Failure }).strict(),
  z.object({ type: z.literal("step.retryScheduled"), ...Step, error: Failure, delayMs: z.number().int().min(0).max(maxDurationMs) }).strict(),
  z.object({ type: z.literal("run.retryRequested"), stepId: StepID }).strict(),
  z.object({ type: z.literal("run.completed"), result: z.json() }).strict(),
  z.object({ type: z.literal("run.failed"), error: Failure }).strict(),
  z.object({ type: z.literal("run.cancelRequested") }).strict(),
  z.object({ type: z.literal("run.cancelled") }).strict(),
  z.object({ type: z.literal("run.expired"), reason: z.string().min(1) }).strict(),
  z.object({ type: z.literal("run.unknown"), error: Failure }).strict(),
  // The code no longer matches the recorded steps; the run waits for Restart or Cancel.
  z.object({ type: z.literal("run.attention"), error: Failure }).strict(),
  z.object({ type: z.literal("run.restarted") }).strict(),
]);

const Envelope = z.object({ version: z.literal(1), seq: z.number().int().positive(), at: z.iso.datetime() }).strict();

export type EventInput = z.infer<typeof EventInputSchema>;
export type JournalEvent = EventInput & z.infer<typeof Envelope>;
export type JournalFailure = z.infer<typeof Failure>;
export type JSONValue = z.infer<ReturnType<typeof z.json>>;

export class JournalError extends Error {
  constructor(readonly code: "NOT_DUE" | "INVALID_EVENT" | "CORRUPT_JOURNAL" | "LOCKED" | "CLOSED" | "POISONED" | "LIMIT", message: string) {
    super(message);
    this.name = "JournalError";
  }
}

export function eventInput(value: unknown): EventInput {
  const parsed = EventInputSchema.safeParse(value);
  if (!parsed.success) throw new JournalError("INVALID_EVENT", parsed.error.message);
  return JSON.parse(JSON.stringify(parsed.data)) as EventInput;
}

export function storedEvent(value: unknown): JournalEvent {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("Expected an event object.");
  const { version, seq, at, ...input } = value as Record<string, unknown>;
  return { ...eventInput(input), ...Envelope.parse({ version, seq, at }) };
}
