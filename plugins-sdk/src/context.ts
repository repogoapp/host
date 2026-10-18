import type { Bucket } from "./buckets";
import type { Collection } from "./collections";
import type { Kv } from "./kv";
import type { Stream } from "./streams";
import type { MethodKind } from "./define";

/** An alert for the user's phones: what it says, and the page a tap opens. */
export type Notice = {
  title: string;
  body: string;
  /** A page target, `.repogo/tabs/vercel.md?project=a`; a tap opens it. */
  page?: string;
  /** Alerts sharing a collapse id replace one another; defaults to the plugin and title. */
  collapse?: string;
};

/** One method as `ctx.plugins()` and the agent's `plugins` tool list it. */
export type ManifestMethod = {
  /** `<plugin>.<method>`, the name `ctx.call` takes. */
  name: string;
  kind: MethodKind;
  description: string | null;
  /** What a card asks before running it; null runs on tap. */
  confirm: string | null;
  /** JSON Schema of the input, and of the output when the method declares one. */
  input: unknown;
  output: unknown;
};

/** One cron and how it's going: times in ms, and what its last run threw. */
export type ManifestCron = {
  method: string;
  schedule: string;
  timezone: string;
  nextRunAt: number | null;
  lastRunAt: number | null;
  lastError: string | null;
  /** How long its last run took: a run far past its interval blocked or waited on something. */
  lastDurationMs: number | null;
  running: boolean;
  runs: number;
};

/** Where a plugin's buckets and streams sit on disk, for an agent's own file tools, and its kv names. */
export type ManifestStorage = {
  buckets: { name: string; path: string }[];
  kv: { name: string }[];
  streams: { name: string; path: string }[];
};

export type Manifest = {
  plugins: {
    name: string;
    description: string | null;
    methods: ManifestMethod[];
    crons: ManifestCron[];
    storage: ManifestStorage;
  }[];
};

// What every method and handler receives. Kept small on purpose: it is the
// whole surface an agent has to learn.
/** Who started a call: a tap or a card's query, an agent's `plugin_call`, a cron, or plugin code with none of those behind it. */
export type Caller = "card" | "agent" | "cron" | "code";

export type Context = {
  plugin: string;
  /** Who started the outermost call; `ctx.call` passes it on. */
  caller: Caller;
  /** The project's checkout, for a plugin that reads the project's files. */
  root: string;
  collections: Record<string, Collection<any>>;
  buckets: Record<string, Bucket<any>>;
  kv: Record<string, Kv<any>>;
  streams: Record<string, Stream<any>>;
  call: (method: string, input?: unknown) => Promise<unknown>;
  emit: (name: string, payload: unknown) => Promise<void>;
  /**
   * Runs one of the host's methods, the ones agents and cards can call:
   * `ctx.host("git.status", { path: ctx.root })`. Its answer doesn't re-run a query.
   */
  host: (method: string, params?: unknown) => Promise<unknown>;
  /**
   * The plugins beside this one, in the same folder (the project's, or the
   * environment's), with their methods: what `call` can reach.
   */
  plugins: () => Manifest["plugins"];
  /** Aborts when a stream's last watcher leaves; a query or mutation's never does. */
  signal: AbortSignal;
  /** Sends an alert to every phone paired with this environment. */
  notify: (notice: Notice) => void;
  log: (...args: unknown[]) => void;
};
