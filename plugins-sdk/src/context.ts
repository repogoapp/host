import type { Bucket } from "./buckets";
import type { Collection } from "./collections";
import type { Kv } from "./kv";
import type { Stream } from "./streams";
import type { HTTPAccess, MethodKind } from "./define";
import type { Duration } from "./duration";

/** An alert for the user's phones: what it says, and the page a tap opens. */
export type Notice = {
  title: string;
  body: string;
  /** A page target, `.repogo/tabs/vercel.md?project=a`; a tap opens it. */
  page?: string;
  /** An `https` or `itms-services` link a tap opens instead of the page, such as a build's install. */
  url?: string;
  /** Alerts sharing a collapse id replace one another; defaults to the plugin and title. */
  collapse?: string;
};

/** A Live Activity's color: the cards' named colors, less `secondary`. */
export type ActivityTint =
  | "primary" | "accent" | "success" | "warning" | "danger"
  | "blue" | "green" | "orange" | "red" | "purple" | "pink" | "teal" | "yellow" | "gray";

/** A segment per step, or one bar. */
export type ActivityProgress =
  | {
      kind: "steps";
      /** 1 to 8 names of at most 24 characters. */
      steps: string[];
      /** The active step, 0 to `steps.length`; the ones before it are complete. */
      current: number;
    }
  | {
      kind: "bar";
      /** 0 to 1. */
      value: number;
    };

/** The card's clock, which the phone ticks with no pushes. */
export type ActivityTimer =
  | {
      kind: "elapsed";
      /** Unix ms; the activity's first start unless said. */
      startedAt?: number;
    }
  | {
      kind: "countdown";
      /** Unix ms; reaching it ends nothing. */
      endsAt: number;
    }
  | { kind: "none" };

/**
 * A Live Activity on the user's phones, sent whole each time: the same `id`
 * updates it, a field left out takes its default, and status `done` or
 * `failed` ends it with this state.
 */
export type Activity = {
  /** The activity's id within the plugin, at most 64 bytes. */
  id: string;
  /** At most 60 characters, as a notice's title. */
  title: string;
  /** At most 120 characters, as a notice's body. */
  body?: string;
  /** A page target of the calling project, `.repogo/tabs/deploys.ui?project=web`; a tap opens it. */
  page?: string;
  /** An SF Symbol name, `shippingbox`; `circle.dotted`, or a check or a cross once ended, unless said. */
  symbol?: string;
  /** Colors the symbol, the progress and the label; white, or green or red once ended, unless said. */
  tint?: ActivityTint;
  /** The card's value, at most 24 characters; "2 of 4", "40%" or the status unless said, and "" hides it. */
  label?: string;
  /** The Dynamic Island's value, at most 10 characters; the label, or "2/4", unless said. */
  compactLabel?: string;
  /** No progress unless said. */
  progress?: ActivityProgress;
  /** Elapsed since the first start unless said. */
  timer?: ActivityTimer;
  /** `running` unless said; `done` and `failed` end it. */
  status?: "running" | "done" | "failed";
};

/** One method as `ctx.plugins()` and the agent's `plugins` tool list it. */
export type ManifestMethod = {
  /** `<plugin>.<method>`, the name `ctx.call` takes. */
  name: string;
  kind: MethodKind;
  description: string | null;
  /** What a card asks before running it; null runs on tap. */
  confirm: string | null;
  /** Who may call it at the environment's own address; null, no one. */
  http: HTTPAccess | null;
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

/**
 * One schedule the host gives a Worker, from a line of `~/.repogo/crons.json`: call
 * `<plugin>.<method>` with `{}` when `schedule` comes due in `timezone` (the environment's).
 */
export type CronEntry = { plugin: string; method: string; schedule: string; timezone?: string };

/** A schedule and how it's going; `error` says why it couldn't be set, and then nothing runs. */
export type CronStatus = ManifestCron & { plugin: string; error: string | null };

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
export type Caller = "card" | "agent" | "cron" | "code" | "http";

/** How `ctx.agent` runs a chat: which agent, in which project, and how long to wait for its answer. */
export type AgentOptions = {
  /** `claude` or `codex`, as `agents.list` names them. */
  agent: string;
  /** The project the chat runs in; the calling project unless said. */
  project?: string;
  title?: string;
  /** How long to wait for the answer; 10 minutes unless said. */
  timeout?: Duration;
};

/** What the agent answered, and the chat it answered in, to open or message later. */
export type AgentReply = { chatId: string; text: string };

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
   * Runs a tool of an MCP server the user switched on for the calling project:
   * `ctx.mcp("stripe", "stripe_api_read", { … })`, answering the tool's result.
   * A query may run only read-only tools; a destructive tool runs only from a
   * method with a `confirm`. Its answer doesn't re-run a query.
   */
  mcp: (server: string, tool: string, input?: unknown) => Promise<unknown>;
  /**
   * The plugins beside this one, in the same folder (the project's, or the
   * environment's), with their methods: what `call` can reach.
   */
  plugins: () => Manifest["plugins"];
  /** Aborts when a stream's last watcher leaves; a query or mutation's never does. */
  signal: AbortSignal;
  /** Sends an alert to every phone paired with this environment. */
  notify: (notice: Notice) => void;
  /**
   * Shows or updates a Live Activity on every paired phone:
   * `await ctx.activity({ id: "deploy", title: "Deploying", progress: { kind: "steps", steps: ["Build", "Ship"], current: 0 } })`.
   * A plugin shows at most two at once.
   */
  activity: (activity: Activity) => Promise<void>;
  /**
   * Starts a chat with `prompt` and waits for the agent's whole answer:
   * `await ctx.agent("Why is the build failing?", { agent: "claude" })`.
   * Throws once `timeout` passes, naming the chat, which keeps running.
   */
  agent: (prompt: string, options: AgentOptions) => Promise<AgentReply>;
  /** The same as `console.log`: every console line is kept with the plugin and the call that wrote it. */
  log: (...args: unknown[]) => void;
};
