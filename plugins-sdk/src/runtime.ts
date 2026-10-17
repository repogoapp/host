import { SQL } from "bun";
import { AsyncLocalStorage } from "node:async_hooks";
import { mkdir, readdir } from "node:fs/promises";
import { join } from "node:path";
import { z } from "zod";
import { Collection } from "./collections";
import type { Context, Notice } from "./context";
import type { PluginDefinition, MethodKind } from "./define";
import { MethodError } from "./errors";
import { EventBus } from "./events";
import { currentAccess, tracked } from "./tracking";

export type RegisteredMethod = {
  name: string;
  plugin: string;
  kind: MethodKind;
  description?: string;
  confirm?: string;
  input: z.ZodType;
  output?: z.ZodType;
  /** `raw` is the input as sent, before defaults filled it in. */
  run: (input: unknown, raw: unknown) => Promise<unknown>;
};

export type ManifestMethod = {
  name: string;
  kind: MethodKind;
  description: string | null;
  /** What a card asks before running it; null runs on tap. */
  confirm: string | null;
  input: unknown;
  output: unknown;
};

export type Manifest = {
  plugins: { name: string; description: string | null; methods: ManifestMethod[] }[];
};

/** A call's result with the data it read (what a query depends on) and wrote (what it invalidates). */
export type CallResult = { result: unknown; reads: string[]; invalidates: string[] };

type InvalidationListener = (tags: string[]) => void;

/** A notice as the host hears it: which plugin, from which project. */
export type SentNotice = Notice & { plugin: string; root: string };
type NoticeListener = (notice: SentNotice) => void;

/** What an agent's call to a stream gets: the items within this long, or this many. */
const sampleMs = Number(process.env.REPOGO_PLUGINS_SAMPLE_MS ?? 5000);
const sampleLimit = 200;

/** A call that isn't a stream has a signal that never aborts. */
const never = new AbortController().signal;

const pluginName = /^[a-z][a-z0-9-]*$/;
const dbName = /^[a-z][a-z0-9.-]*$/;

// Loads every plugin in the plugins dir, opens the database its collections
// name, builds its context, and registers its methods. The registry is what the
// agent's tools, cards and schedules all call.
export class Runtime {
  readonly events = new EventBus();
  readonly methods = new Map<string, RegisteredMethod>();
  readonly plugins = new Map<string, PluginDefinition>();
  private databases = new Map<string, SQL>();
  private invalidationListeners = new Set<InvalidationListener>();
  private noticeListeners = new Set<NoticeListener>();
  pluginsDir: string | null = null;

  private constructor(
    private dataDir: string,
    private root: string,
  ) {}

  /** The caller's project while a call runs, for plugins that serve every project. */
  private callRoot = new AsyncLocalStorage<string>();
  /** A running stream's signal, `ctx.signal` to the code it runs. */
  private callSignal = new AsyncLocalStorage<AbortSignal>();

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
    const database = plugin.db ?? name;
    if (!dbName.test(database)) throw new Error(`a plugin's db is lowercase letters, digits, dots and hyphens: ${database}`);

    const db: Context["db"] = {};
    const collections = Object.entries(plugin.collections ?? {});
    // Only collections need a file; plugins naming the same db share it on purpose.
    const sql = collections.length > 0 ? await this.database(database) : null;
    for (const [collectionName, schema] of collections) {
      const collection = new Collection(sql!, this.events, name, database, collectionName, schema);
      await collection.setup();
      db[collectionName] = collection;
      this.registerCrud(name, collectionName, collection, schema);
    }

    const runtime = this;
    const ctx: Context = {
      plugin: name,
      get root() { return runtime.callRoot.getStore() ?? runtime.root; },
      get signal() { return runtime.callSignal.getStore() ?? never; },
      db,
      call: async (method, input) => (await this.call(method.includes(".") ? method : `${name}.${method}`, input)).result,
      emit: (event, payload) => this.events.emit(`${name}.${event}`, payload),
      notify: (notice) => {
        const sent = { ...notice, plugin: name, root: runtime.callRoot.getStore() ?? runtime.root };
        for (const listener of runtime.noticeListeners) listener(sent);
      },
      log: (...args) => console.error(`[${name}]`, ...args),
    };

    for (const [methodName, method] of Object.entries(plugin.methods ?? {})) {
      this.register({
        name: `${name}.${methodName}`,
        plugin: name,
        kind: method.kind,
        description: method.description,
        confirm: method.confirm,
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
  /**
   * `confirmed` is for a call from a tap: false until the user has answered
   * the method's `confirm`. Code calling code (`ctx.call`) leaves it out.
   * `root` is the caller's project, for plugins loaded from the environment's
   * own folder; code calling code inherits it.
   */
  async call(name: string, input: unknown = {}, options: { expect?: MethodKind; confirmed?: boolean; root?: string } = {}): Promise<CallResult> {
    if (options.root && options.root !== this.callRoot.getStore()) {
      const { root, ...rest } = options;
      return this.callRoot.run(root, () => this.call(name, input, rest));
    }
    const method = this.methods.get(name);
    if (!method) throw new MethodError("NOT_FOUND", `There is no method ${name}.`);
    if (method.confirm && options.confirmed === false) throw new MethodError("CONFIRM", method.confirm);
    if ((options.expect === "query" || currentAccess()?.readOnly) && method.kind === "mutation") {
      throw new MethodError("FORBIDDEN", `${name} changes data, so it only runs when the user acts.`);
    }
    if (method.kind === "stream") return { result: await this.sample(name, input), reads: [], invalidates: [] };
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

  /**
   * Runs a stream method, passing each item it yields to `onItem` until it
   * returns, throws or `signal` aborts. Bad input throws here, before it starts.
   */
  stream(name: string, input: unknown, options: { root?: string; signal: AbortSignal }, onItem: (item: unknown) => void): Promise<void> {
    const method = this.methods.get(name);
    if (!method) throw new MethodError("NOT_FOUND", `There is no method ${name}.`);
    if (method.kind !== "stream") throw new MethodError("VALIDATION", `${name} is a ${method.kind}, not a stream.`);
    const raw = input ?? {};
    const parsed = method.input.parse(raw);
    const root = options.root ?? this.callRoot.getStore() ?? this.root;
    return this.callRoot.run(root, () => this.callSignal.run(options.signal, async () => {
      // Read-only like a query: a stream a card starts on its own may not write.
      const run = await tracked(true, (tags) => this.notifyInvalidated(tags), async () => {
        const items = await method.run(parsed, raw);
        if (!isAsyncIterable(items)) throw new MethodError("INTERNAL", `${name} is a stream but returned no async iterable.`);
        for await (const item of items) {
          if (options.signal.aborted) break;
          onItem(item);
        }
      });
      if (!run.ok && !options.signal.aborted) throw run.error;
    }));
  }

  /** A stream called once, by an agent or by code: what arrives within `sampleMs`, at most `sampleLimit` items. */
  private async sample(name: string, input: unknown) {
    const items: unknown[] = [];
    const stop = new AbortController();
    const timer = setTimeout(() => stop.abort(), sampleMs);
    try {
      const stopped = new Promise<void>((resolve) => stop.signal.addEventListener("abort", () => resolve()));
      const ended = this.stream(name, input, { signal: stop.signal }, (item) => {
        items.push(item);
        if (items.length >= sampleLimit) stop.abort();
      });
      await Promise.race([ended, stopped]);
      // Done is the stream ending on its own, not being cut off here.
      return { items, done: !stop.signal.aborted };
    } finally {
      clearTimeout(timer);
      stop.abort();
    }
  }

  /** Notices plugins send, for the host to push to the phones. */
  onNotify(listener: NoticeListener) {
    this.noticeListeners.add(listener);
    return () => this.noticeListeners.delete(listener);
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
          confirm: method.confirm ?? null,
          input: z.toJSONSchema(method.input, { io: "input", unrepresentable: "any" }),
          output: method.output ? z.toJSONSchema(method.output, { io: "output", unrepresentable: "any" }) : null,
        })),
    }));
    return { plugins };
  }

  close() {
    for (const sql of this.databases.values()) sql.close();
  }

  private async database(name: string): Promise<SQL> {
    let sql = this.databases.get(name);
    if (sql) return sql;
    sql = new SQL(`sqlite://${join(this.dataDir, `${name}.db`)}`);
    // Other Workers open the same file: WAL lets them read while one writes,
    // and a writer waits for the other's lock.
    await sql.unsafe("pragma journal_mode = wal");
    await sql.unsafe("pragma busy_timeout = 5000");
    this.databases.set(name, sql);
    return sql;
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
      confirm?: string,
    ) => this.register({
      name: `${prefix}.${method}`, plugin, kind, description, confirm, input, output,
      run: async (value, raw) => run(value, raw),
    });

    crud("list", "query", z.object({ limit: z.number().int().positive().max(500).optional() }), z.array(record),
      `List ${name} records, newest first`, (input) => collection.list(input));
    crud("get", "query", id, record.nullable(), `Get one ${name} record by id, or null`, (input) => collection.get(input.id));
    crud("create", "mutation", schema, record, `Create a ${name} record`, (input) => collection.create(input));
    // Only the fields the caller sent change: a default never overwrites a field left out.
    crud("update", "mutation", id.extend(schema.partial().shape), record, `Update fields on a ${name} record`,
      ({ id, ...fields }, raw) =>
        collection.update(id, Object.fromEntries(Object.entries(fields).filter(([key]) => Object.hasOwn(raw ?? {}, key)))),
    );
    // A delete can't be undone, so a card asks first.
    crud("delete", "mutation", id, z.null(), `Delete a ${name} record by id`, async (input) => {
      await collection.delete(input.id);
      return null;
    }, "Delete this record?");
  }
}

function isAsyncIterable(value: unknown): value is AsyncIterable<unknown> {
  return typeof (value as AsyncIterable<unknown> | null)?.[Symbol.asyncIterator] === "function";
}
