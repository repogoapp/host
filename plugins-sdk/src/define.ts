import type { z } from "zod";
import type { Context } from "./context";

export type Collections = Record<string, z.ZodObject>;

/**
 * A query only reads, so cards may run it on their own and again when its data
 * changes. A mutation writes, and runs only when the user acts. Unmarked
 * methods are mutations: repeating one is never assumed safe.
 */
export type MethodKind = "query" | "mutation";

export type Method<I extends z.ZodType = z.ZodType, O extends z.ZodType = z.ZodType> = {
  input: I;
  /** What it returns; the agent and cards read field names from it. */
  output?: O;
  kind: MethodKind;
  description?: string;
  /**
   * For what can't be undone: the question a card's button asks before it
   * runs, such as "Delete every item?". Methods without one run on tap.
   */
  confirm?: string;
  run: (input: z.infer<I>, ctx: Context) => unknown | Promise<unknown>;
};

export type Handler = (payload: any, ctx: Context) => unknown | Promise<unknown>;

export type PluginDefinition = {
  /** One line, shown to the agent in the index of plugins. */
  description?: string;
  /**
   * The data file the collections live in, `<db>.db` in the data dir; the
   * plugin's folder name when left out. A project's own data is `<project>.<name>`
   * (`repogo.plans`); a bare name (`todos`) is shared by every plugin that names it.
   */
  db?: string;
  collections?: Collections;
  methods?: Record<string, Method<any, any>>;
  on?: Record<string, Handler>;
};

export function definePlugin<T extends PluginDefinition>(plugin: T): T {
  return plugin;
}

export function method<I extends z.ZodType, O extends z.ZodType = z.ZodType>(
  input: I,
  run: (input: z.infer<I>, ctx: Context) => unknown | Promise<unknown>,
  options: { kind?: MethodKind; output?: O; description?: string; confirm?: string } = {},
): Method<I, O> {
  return {
    input, run, kind: options.kind ?? "mutation", output: options.output,
    description: options.description, confirm: options.confirm,
  };
}
