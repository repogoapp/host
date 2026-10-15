import { SQL } from "bun";
import { mkdir, readdir } from "node:fs/promises";
import { join } from "node:path";
import { z } from "zod";
import { Collection } from "./collections";
import type { Context } from "./context";
import type { PluginDefinition, MethodKind } from "./define";
import { MethodError } from "./errors";
import { EventBus } from "./events";
import { currentAccess, tracked } from "./tracking";

export type RegisteredMethod = {
  name: string;
  plugin: string;
  kind: MethodKind;
  description?: string;
  input: z.ZodType;
  output?: z.ZodType;
  /** `raw` is the input as sent, before defaults filled it in. */
  run: (input: unknown, raw: unknown) => Promise<unknown>;
};

export type ManifestMethod = {
  name: string;
  kind: MethodKind;
  description: string | null;
  input: unknown;
  output: unknown;
};

export type Manifest = {
  plugins: { name: string; description: string | null; methods: ManifestMethod[] }[];
};

/** A call's result with the data it read (what a query depends on) and wrote (what it invalidates). */
export type CallResult = { result: unknown; reads: string[]; invalidates: string[] };

type InvalidationListener = (tags: string[]) => void;

const pluginName = /^[a-z][a-z0-9-]*$/;

// Loads every plugin in the plugins dir, gives each its own database, collections
// and context, and registers its methods. The registry is what the agent's
// tools, cards and schedules all call.
export class Runtime {
  readonly events = new EventBus();
  readonly methods = new Map<string, RegisteredMethod>();
  readonly plugins = new Map<string, PluginDefinition>();
  private databases = new Map<string, SQL>();
  private invalidationListeners = new Set<InvalidationListener>();
  pluginsDir: string | null = null;

  private constructor(
    private dataDir: string,
    private root: string,
  ) {}

  /** `root` is the project's checkout, `ctx.root` to its plugins; the working directory when left out. */
  static async open(options: { dataDir: string; root?: string }) {
    await mkdir(options.dataDir, { recursive: true });
    return new Runtime(options.dataDir, options.root ?? process.cwd());
  }

  async loadDir(pluginsDir: string) {
    this.pluginsDir = pluginsDir;
    const entries = await readdir(pluginsDir, { withFileTypes: true });
    for (const entry of entries) {
      if (!entry.isDirectory() || !pluginName.test(entry.name)) continue;
      const module = await import(join(pluginsDir, entry.name, "app.ts"));
      await this.add(entry.name, module.default);
    }
  }

  async add(name: string, plugin: PluginDefinition) {
    if (!pluginName.test(name)) throw new Error(`plugin names are lowercase letters, digits and hyphens: ${name}`);
    // One database file per plugin: a plugin can only reach another's data through its methods.
    const sql = new SQL(`sqlite://${join(this.dataDir, `${name}.db`)}`);
    // Two checkouts of a project run in two Workers over the same file: WAL lets
    // them read while one writes, and a writer waits for the other's lock.
    await sql.unsafe("pragma journal_mode = wal");
    await sql.unsafe("pragma busy_timeout = 5000");
    this.databases.set(name, sql);

    const db: Context["db"] = {};
    for (const [collectionName, schema] of Object.entries(plugin.collections ?? {})) {
      const collection = new Collection(sql, this.events, name, collectionName, schema);
      await collection.setup();
      db[collectionName] = collection;
      this.registerCrud(name, collectionName, collection, schema);
    }

    const ctx: Context = {
      plugin: name,
      root: this.root,
      db,
      call: async (method, input) => (await this.call(method.includes(".") ? method : `${name}.${method}`, input)).result,
      emit: (event, payload) => this.events.emit(`${name}.${event}`, payload),
      log: (...args) => console.error(`[${name}]`, ...args),
    };

    for (const [methodName, method] of Object.entries(plugin.methods ?? {})) {
      this.register({
        name: `${name}.${methodName}`,
        plugin: name,
        kind: method.kind,
        description: method.description,
        input: method.input,
        output: method.output,
        run: async (input) => method.run(input, ctx),
      });
    }

    for (const [event, handler] of Object.entries(plugin.on ?? {})) {
      this.events.on(`${name}.${event}`, (e) => handler(e.payload, ctx));
    }

    this.plugins.set(name, plugin);
  }

  /**
   * Calls a method and reports what it read and wrote. `expect: "query"` is how
   * a card runs a method on its own: it refuses anything that might write, and
   * so does everything the query calls.
   */
  async call(name: string, input: unknown = {}, options: { expect?: MethodKind } = {}): Promise<CallResult> {
    const method = this.methods.get(name);
    if (!method) throw new MethodError("NOT_FOUND", `There is no method ${name}.`);
    if ((options.expect === "query" || currentAccess()?.readOnly) && method.kind !== "query") {
      throw new MethodError("FORBIDDEN", `${name} changes data, so it only runs when the user acts.`);
    }
    const raw = input ?? {};
    const parsed = method.input.parse(raw);
    // A query is read-only however it's called, and so is everything it calls.
    const run = await tracked(method.kind === "query", (tags) => this.notifyInvalidated(tags), async () => {
      const result = await method.run(parsed, raw);
      await this.events.emit(name, { input: parsed, result });
      return result;
    });
    // Announced by the outermost call, and even when the method threw after writing.
    const invalidates = run.outermost ? [...run.access.writes] : [];
    if (invalidates.length > 0) this.notifyInvalidated(invalidates);
    if (!run.ok) throw run.error;
    return { result: run.value ?? null, reads: [...run.access.reads], invalidates };
  }

  /** Tags written by any call, as they commit; cards refetch the queries that read them. */
  onInvalidate(listener: InvalidationListener) {
    this.invalidationListeners.add(listener);
    return () => this.invalidationListeners.delete(listener);
  }

  manifest(): Manifest {
    const plugins = [...this.plugins.entries()].map(([name, plugin]) => ({
      name,
      description: plugin.description ?? null,
      methods: [...this.methods.values()]
        .filter((method) => method.plugin === name)
        .map((method) => ({
          name: method.name,
          kind: method.kind,
          description: method.description ?? null,
          input: z.toJSONSchema(method.input, { io: "input", unrepresentable: "any" }),
          output: method.output ? z.toJSONSchema(method.output, { io: "output", unrepresentable: "any" }) : null,
        })),
    }));
    return { plugins };
  }

  close() {
    for (const sql of this.databases.values()) sql.close();
  }

  private notifyInvalidated(tags: string[]) {
    for (const listener of this.invalidationListeners) listener(tags);
  }

  private register(method: RegisteredMethod) {
    this.methods.set(method.name, method);
  }

  // Every collection gets list/get/create/update/delete, so a data-only plugin
  // needs no methods to be read and changed by the agent and by cards.
  private registerCrud(plugin: string, name: string, collection: Collection<any>, schema: z.ZodObject) {
    const prefix = `${plugin}.${name}`;
    const id = z.object({ id: z.string() });
    const record = schema.extend({ id: z.string(), createdAt: z.string(), updatedAt: z.string() });
    const crud = (
      method: string,
      kind: MethodKind,
      input: z.ZodType,
      output: z.ZodType,
      description: string,
      run: (input: any, raw: any) => unknown,
    ) => this.register({ name: `${prefix}.${method}`, plugin, kind, description, input, output, run: async (value, raw) => run(value, raw) });

    crud("list", "query", z.object({ limit: z.number().int().positive().max(500).optional() }), z.array(record),
      `List ${name} records, newest first`, (input) => collection.list(input));
    crud("get", "query", id, record.nullable(), `Get one ${name} record by id, or null`, (input) => collection.get(input.id));
    crud("create", "mutation", schema, record, `Create a ${name} record`, (input) => collection.create(input));
    // Only the fields the caller sent change: a default never overwrites a field left out.
    crud("update", "mutation", id.extend(schema.partial().shape), record, `Update fields on a ${name} record`,
      ({ id, ...fields }, raw) =>
        collection.update(id, Object.fromEntries(Object.entries(fields).filter(([key]) => Object.hasOwn(raw ?? {}, key)))),
    );
    crud("delete", "mutation", id, z.null(), `Delete a ${name} record by id`, async (input) => {
      await collection.delete(input.id);
      return null;
    });
  }
}
