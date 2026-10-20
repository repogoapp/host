import { z } from "zod";
import { parseDuration, type Duration } from "../duration";

const duration = z.custom<Duration>(value => typeof value === "number" || typeof value === "string");
const StepOptionsSchema = z.object({
  kind: z.enum(["query", "mutation"]).optional(), attempts: z.number().optional(),
  timeout: duration.optional(),
  backoff: z.object({ initial: duration.optional(), factor: z.number().optional(), max: duration.optional() }).strict().optional(),
}).strict();
export type StepOptions = z.infer<typeof StepOptionsSchema>;

export const maxDurationMs = 365 * 86_400_000;
export const DurationMsSchema = z.number().int().positive().max(maxDurationMs);
export const StepPolicySchema = z.object({
  kind: z.enum(["query", "mutation"]),
  attempts: z.number().int().min(1).max(100),
  timeoutMs: DurationMsSchema,
  backoff: z.object({ initialMs: DurationMsSchema, factor: z.number().min(1).max(1000), maxMs: DurationMsSchema }).strict(),
}).strict().refine(value => value.kind === "query" || value.attempts === 1, "Only query steps may retry.")
  .refine(value => value.backoff.initialMs <= value.backoff.maxMs, "Initial backoff must not exceed its maximum.");
export type StepPolicy = z.infer<typeof StepPolicySchema>;

export function durationMs(value: Duration): number {
  return DurationMsSchema.parse(Math.ceil(parseDuration(value)));
}

export function stepPolicy(options: StepOptions = {}): StepPolicy {
  options = StepOptionsSchema.parse(options);
  return StepPolicySchema.parse({
    kind: options.kind ?? "mutation", attempts: options.attempts ?? 1,
    timeoutMs: durationMs(options.timeout ?? "5m"),
    backoff: {
      initialMs: durationMs(options.backoff?.initial ?? "2s"),
      factor: options.backoff?.factor ?? 2, maxMs: durationMs(options.backoff?.max ?? "1m"),
    },
  });
}

export function retryDelay(policy: StepPolicy, attempt: number, random: () => number): number {
  const sample = random();
  if (!Number.isFinite(sample) || sample < 0 || sample >= 1) throw new Error("Retry entropy must be in [0, 1).");
  const maximum = Math.min(policy.backoff.maxMs, policy.backoff.initialMs * policy.backoff.factor ** (attempt - 1));
  return Math.floor(maximum * sample);
}
