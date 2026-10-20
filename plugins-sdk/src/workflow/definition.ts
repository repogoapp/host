import type { z } from "zod";
import type { Context } from "../context";
import type { Duration } from "../duration";
import type { StepContext as AttemptContext } from "./attempts";
import type { StepOptions } from "./policy";
import type { JSONValue } from "./events";
import { attemptScope } from "./scope";
import type { RunContext } from "./runner";

export type StepContext = AttemptContext & { runId: string; idempotencyKey: string };
export type WorkflowSteps = {
  run<T extends JSONValue>(name: string, callback: (ctx: StepContext) => T | Promise<T>, options?: StepOptions): Promise<T>;
};
export type WorkflowContext = Context & Omit<RunContext, "step"> & { runId: string; step: WorkflowSteps };

export function workflowContext(base: Context, run: RunContext & { root: string; runId: string }): WorkflowContext {
  return {
    ...base, ...run,
    get signal() { return attemptScope.getStore()?.signal ?? run.signal; },
    step: { run: (name, callback, options) => run.step.run(name, context => callback({ ...context, runId: run.runId, idempotencyKey: `${run.runId}:${name}` }), options) },
  };
}
export type WorkflowOptions<O extends z.ZodType = z.ZodType> = {
  description?: string;
  output?: O;
  confirm?: string;
  timeout?: Duration;
  maxOffline?: Duration;
  retention?: Duration;
};
export type Workflow<I extends z.ZodType = z.ZodType, O extends z.ZodType = z.ZodType> = WorkflowOptions<O> & {
  input: I;
  run: (input: z.infer<I>, ctx: WorkflowContext) => unknown | Promise<unknown>;
};

export function workflow<I extends z.ZodType, O extends z.ZodType = z.ZodType>(
  input: I,
  run: (input: z.infer<I>, ctx: WorkflowContext) => unknown | Promise<unknown>,
  options: WorkflowOptions<O> = {},
): Workflow<I, O> {
  return { input, run, ...options };
}
