import { z } from "zod";
import type { Context } from "../context";
import type { RegisteredMethod } from "../runtime";
import type { Workflow, WorkflowContext } from "./definition";
import type { WorkflowBridge, WorkflowRequest } from "./contracts";
import { RunSummarySchema } from "./contracts";
import { MethodError } from "../errors";
import { recordRead } from "../tracking";
import { durationMs } from "./policy";

export function workflowTag(namespace: string) { return `${namespace}.workflows`; }

export function workflowMethods(
  plugin: string, namespace: string, name: string, definition: Workflow, ctx: Context,
  bridge: WorkflowBridge | undefined, directory: () => string | null,
): RegisteredMethod[] {
  const invoke = (operation: WorkflowRequest["operation"], input: unknown) => {
    if (!bridge || !directory()) throw new MethodError("INTERNAL", "Durable workflows run through the plugins service with source files.");
    return bridge({ operation, dir: directory()!, plugin, namespace, workflow: name, root: ctx.root, caller: ctx.caller, input: input as Record<string, unknown>, settings: workflowSettings(definition) });
  };
  const id = z.object({ id: z.string().uuid() }).strict();
  const spec = (method: WorkflowRequest["operation"], kind: "query" | "mutation", input: z.ZodType, description: string, confirm?: string, output?: z.ZodType): RegisteredMethod => ({
    name: `${plugin}.${name}.${method}`, plugin, kind, input, description, confirm, output,
    run: async (input, raw) => {
      if (kind === "query") recordRead(workflowTag(namespace));
      const request = method === "start"
        ? { ...(input as Record<string, unknown>), input: (raw as Record<string, unknown>).input }
        : input;
      return invoke(method, request);
    },
  });
  return [
    spec("list", "query", z.object({ limit: z.number().int().min(1).max(100).default(20), status: z.string().optional(), cursor: z.string().optional() }).strict(), "List workflow runs", undefined, z.object({ items: z.array(RunSummarySchema), cursor: z.string().nullable() })),
    spec("get", "query", id, "Get one workflow run"),
    spec("start", "mutation", z.object({ input: definition.input, key: z.string().min(1).max(200).optional() }).strict(), definition.description ?? "Start a durable workflow", definition.confirm, RunSummarySchema),
    spec("retry", "mutation", id, "Retry a safely failed query on its original build", undefined, RunSummarySchema),
    spec("cancel", "mutation", id, "Request cancellation of a workflow", undefined, RunSummarySchema),
  ];
}

export function workflowSettings(definition: Workflow) {
  return {
    timeoutMs: durationMs(definition.timeout ?? "30d"), maxOfflineMs: durationMs(definition.maxOffline ?? "14d"),
    retentionMs: durationMs(definition.retention ?? "30d"),
  };
}
