import { AsyncLocalStorage } from "node:async_hooks";
import { mkdir, readdir } from "node:fs/promises";
import { join } from "node:path";
import { z } from "zod";
import { Bucket } from "./buckets";
import { Collection } from "./collections";
import { checkTimezone, nextDue, parseSchedule, type Schedule } from "./cron";
import { Database } from "./database";
import type { Context, Manifest, ManifestCron, Notice } from "./context";

export type { Manifest, ManifestCron, ManifestMethod } from "./context";
import type { PluginDefinition, MethodKind } from "./define";
import { MethodError, toCallError } from "./errors";
import { EventBus } from "./events";
import { bucketMethods, kvMethods, streamMethods, type Generated } from "./generated";
import { Kv } from "./kv";
import type { Store, StoreKind, StoreOptions } from "./store";
import { defaultRetain, Stream } from "./streams";
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

/** A call's result with the data it read (what a query depends on) and wrote (what it invalidates). */
export type CallResult = { result: unknown; reads: string[]; invalidates: string[] };

type InvalidationListener = (tags: string[]) => void;

/** A notice as the host hears it: which plugin, from which project. */
export type SentNotice = Notice & { plugin: string; root: string };
type NoticeListener = (notice: SentNotice) => void;

/** Runs a host method for a plugin's `ctx.host`; the plugins server passes it to the host. */
export type HostCaller = (plugin: string, method: string, params: unknown) => Promise<unknown>;

/** What an agent's call to a stream gets: the items within this long, or this many. */
const sampleMs = Number(process.env.REPOGO_PLUGINS_SAMPLE_MS ?? 5000);
const sampleLimit = 200;

/** A plugin's cron as the runtime keeps it: its schedule read once, and its last run. */
type CronJob = {
  plugin: string;
  /** `<plugin>.<method>`. */
  method: string;
  schedule: string;
  timezone: string;
  parsed: Schedule;
  nextRunAt: number | null;
  lastRunAt: number | null;
  lastError: string | null;
  running: boolean;
  runs: number;
};

/** A due time missed by more than this, as when the machine slept through it, is skipped. */
const cronLateMs = 2 * 60_000;

/** A call that isn't a stream has a signal that never aborts. */
const never = new AbortController().signal;

const pluginName = /^[a-z][a-z0-9-]*$/;
const namespaceName = /^[a-z][a-z0-9.-]*$/;

// Loads every plugin in the plugins dir, opens the namespace its data lives
// in, builds its context, and registers its methods. The registry is what the
// agent's tools, cards and schedules all call.
export class Runtime {
  readonly events = new EventBus();
  readonly methods = new Map<string, RegisteredMethod>();
  readonly plugins = new Map<string, PluginDefinition>();
  readonly crons: CronJob[] = [];
  private databases = new Map<string, Database>();
  /** Every kv, stream and bucket by tag, shared by the plugins that name it, with its schema's JSON. */
  private stores = new Map<string, { store: Store; schema: string }>();
  private invalidationListeners = new Set<InvalidationListener>();
  private noticeListeners = new Set<NoticeListener>();
  pluginsDir: string | null = null;

  private constructor(
    private dataDir: string,
    private root: string,
    private host?: HostCaller,
  ) {}

  /** The caller's project while a call runs, for plugins that serve every project. */
  private callRoot = new AsyncLocalStorage<string>();
  /** A running stream's signal, `ctx.signal` to the code it runs. */
  private callSignal = new AsyncLocalStorage<AbortSignal>();

  /**
   * `root` is the project's checkout, `ctx.root` to its plugins; the working
   * directory when left out. Without `host`, `ctx.host` fails.
   */
  static async open(options: { dataDir: string; root?: string; host?: HostCaller }) {
    await mkdir(options.dataDir, { recursive: true });
    return new Runtime(options.dataDir, options.root ?? process.cwd(), options.host);
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
    const namespace = plugin.namespace ?? name;
    if (!namespaceName.test(namespace)) throw new Error(`a plugin's namespace is lowercase letters, digits, dots and hyphens: ${namespace}`);

    const collections: Context["collections"] = {};
    const buckets: Context["buckets"] = {};
    const kv: Context["kv"] = {};
    const streams: Context["streams"] = {};
    const declared = Object.entries(plugin.collections ?? {});
    // Only data needs a file; plugins naming the same namespace share it on purpose.
    const hasData = declared.length > 0 || [plugin.buckets, plugin.kv, plugin.streams].some((stores) => Object.keys(stores ?? {}).length > 0);
    const file = hasData ? await this.database(namespace) : null;
    for (const [collectionName, schema] of declared) {
      const collection = new Collection(file!, this.events, name, namespace, collectionName, schema);
      await collection.setup();
      collections[collectionName] = collection;
      this.registerCrud(name, collectionName, collection, schema);
    }
    const options: StoreOptions = {
      database: file!, events: this.events, namespace, dir: join(this.dataDir, namespace),
      announce: (tags) => this.notifyInvalidated(tags),
    };
    for (const [storeName, schema] of Object.entries(plugin.kv ?? {})) {
      const store = await this.store(name, "kv", storeName, schema, () => new Kv(options, storeName, schema));
      kv[storeName] = store;
      this.registerGenerated(name, storeName, kvMethods(storeName, store, schema));
    }
    for (const [storeName, definition] of Object.entries(plugin.streams ?? {})) {
      const { item, retain } = "item" in definition ? definition : { item: definition, retain: defaultRetain };
      const store = await this.store(name, "streams", storeName, item, () => new Stream(options, storeName, item, retain), retain);
      streams[storeName] = store;
      this.registerGenerated(name, storeName, streamMethods(storeName, store, item));
    }
    for (const [storeName, schema] of Object.entries(plugin.buckets ?? {})) {
      const store = await this.store(name, "buckets", storeName, schema, () => new Bucket(options, storeName, schema));
      buckets[storeName] = store;
      this.registerGenerated(name, storeName, bucketMethods(storeName, store, schema));
    }

    const runtime = this;
    const ctx: Context = {
      plugin: name,
      get root() { return runtime.callRoot.getStore() ?? runtime.root; },
      get signal() { return runtime.callSignal.getStore() ?? never; },
      collections,
      buckets,
      kv,
      streams,
      call: async (method, input) => (await this.call(method.includes(".") ? method : `${name}.${method}`, input)).result,
      emit: (event, payload) => this.events.emit(`${name}.${event}`, payload),
      host: async (method, params) => {
        if (!runtime.host) throw new MethodError("INTERNAL", "ctx.host runs only under the RepoGo host.");
        return runtime.host(name, method, params ?? {});
      },
      plugins: () => this.manifest().plugins,
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

    // Checked here, so a cron that could never run fails the load with its reason.
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    for (const [index, cron] of (plugin.crons ?? []).entries()) {
      const where = `${name}: crons[${index}]`;
      const method = this.methods.get(`${name}.${cron.method}`);
      if (!method || method.plugin !== name) throw new Error(`${where} names ${cron.method}, which isn't one of ${name}'s methods`);
      if (method.kind === "stream") throw new Error(`${where}: ${cron.method} is a stream; a cron runs a query or a mutation`);
      if (method.confirm) throw new Error(`${where}: ${cron.method} asks before it runs, so nothing may run it on a schedule`);
      if (!method.input.safeParse({}).success) throw new Error(`${where}: ${cron.method} needs input; a cron calls it with {}`);
      let parsed: Schedule;
      try { parsed = parseSchedule(cron.schedule); } catch (error) { throw new Error(`${where}: ${(error as Error).message}`); }
      const timezone = cron.timezone ?? zone;
      try { checkTimezone(timezone); } catch { throw new Error(`${where}: "${timezone}" isn't a time zone`); }
      const nextRunAt = nextDue(parsed, Date.now(), timezone);
      if (nextRunAt === null) throw new Error(`${where}: "${cron.schedule}" is never due`);
      this.crons.push({
        plugin: name, method: method.name, schedule: cron.schedule, timezone, parsed,
        nextRunAt, lastRunAt: null, lastError: null, running: false, runs: 0,
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

  /**
   * Starts every cron due by `now` and returns the runs it started, for a test
   * to wait on. A due time that comes while the job still runs is skipped, as is
   * one missed by more than two minutes: nothing is caught up.
   */
  tickCrons(now = Date.now()): Promise<void>[] {
    const started: Promise<void>[] = [];
    for (const job of this.crons) {
      if (job.nextRunAt === null || job.nextRunAt > now) continue;
      const due = job.nextRunAt;
      job.nextRunAt = nextDue(job.parsed, now, job.timezone);
      // A time that comes while the job still runs, or that was slept through, is skipped.
      if (job.running || now - due > cronLateMs) continue;
      job.running = true;
      job.lastRunAt = now;
      started.push(this.call(job.method, {}).then(
        () => { job.lastError = null; },
        (error) => {
          job.lastError = toCallError(error).message;
          console.error(`[${job.plugin}] cron ${job.method} failed:`, job.lastError);
        },
      ).finally(() => {
        job.running = false;
        job.runs += 1;
      }));
    }
    return started;
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
      crons: this.crons.filter((job) => job.plugin === name).map((job): ManifestCron => ({
        method: job.method, schedule: job.schedule, timezone: job.timezone,
        nextRunAt: job.nextRunAt, lastRunAt: job.lastRunAt, lastError: job.lastError, running: job.running, runs: job.runs,
      })),
      storage: {
        buckets: this.storesOf(name, "buckets").map((store) => ({ name: store.name, path: (store as Bucket<any>).path })),
        kv: this.storesOf(name, "kv").map((store) => ({ name: store.name })),
        streams: this.storesOf(name, "streams").map((store) => ({ name: store.name, path: (store as Stream<any>).path })),
      },
    }));
    return { plugins };
  }

  /** Whether any plugin keeps kv, streams or buckets, which `tickStorage` looks after. */
  get hasStorage() {
    return this.stores.size > 0;
  }

  /**
   * Expires kv keys, trims streams and rescans buckets, each as often as it
   * needs; the Worker calls it every few seconds. One store failing doesn't stop the rest.
   */
  async tickStorage(now = Date.now()) {
    for (const { store } of this.stores.values()) {
      await store.tick(now).catch((error) => console.error(`[${store.kind}] ${store.name}:`, toCallError(error).message));
    }
  }

  close() {
    for (const { store } of this.stores.values()) store.close();
    for (const database of this.databases.values()) database.close();
  }

  private async database(namespace: string): Promise<Database> {
    let database = this.databases.get(namespace);
    if (database) return database;
    await mkdir(join(this.dataDir, namespace), { recursive: true });
    database = await Database.open(join(this.dataDir, namespace, "data.db"));
    this.databases.set(namespace, database);
    return database;
  }

  // One store per tag, set up when the first plugin names it; a second plugin
  // naming it shares it, and must declare the same schema (and retention).
  private async store<T extends Store>(plugin: string, kind: StoreKind, name: string, schema: z.ZodType, make: () => T, extra?: unknown): Promise<T> {
    const signature = JSON.stringify([z.toJSONSchema(schema, { unrepresentable: "any" }), extra ?? null]);
    const made = make();
    const found = this.stores.get(made.tag);
    if (found && found.schema !== signature) {
      throw new Error(`${plugin}: ${kind} ${name} is declared by another plugin in this namespace with a different schema`);
    }
    const store = (found?.store ?? made) as T;
    if (!found) {
      await store.setup();
      this.stores.set(store.tag, { store, schema: signature });
    }
    store.plugins.add(plugin);
    return store;
  }

  private storesOf(plugin: string, kind: StoreKind) {
    return [...this.stores.values()].map(({ store }) => store).filter((store) => store.kind === kind && store.plugins.has(plugin));
  }

  private notifyInvalidated(tags: string[]) {
    for (const listener of this.invalidationListeners) listener(tags);
  }

  private register(method: RegisteredMethod) {
    if (this.methods.has(method.name)) throw new Error(`${method.name} is defined twice; rename the method or the data`);
    this.methods.set(method.name, method);
  }

  private registerGenerated(plugin: string, name: string, methods: Record<string, Generated>) {
    for (const [method, { run, ...spec }] of Object.entries(methods)) {
      this.register({
        name: `${plugin}.${name}.${method}`, plugin, ...spec,
        run: async (input) => run(input, this.callSignal.getStore() ?? never),
      });
    }
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
