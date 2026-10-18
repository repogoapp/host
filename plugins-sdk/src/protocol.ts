import type { Caller, CronEntry } from "./context";
import type { CallError } from "./errors";
import type { Manifest, SentNotice } from "./runtime";

// What the host and the plugins server say to each other over the socket: one JSON
// object per line. A request carries an `id` and gets one reply with that `id`;
// a push (`data`) has none.

/**
 * Every request names the code it runs, `dir`: a `.repogo/plugins` folder, or
 * the environment's own under `~/.repogo`. `root` is the caller's project; a
 * project's folder leaves it out and its checkout is the root.
 */
export type Target = { dir: string; root?: string };

export type Request =
  | { id: number; method: "list"; params: Target }
  /** `confirmed`: the user has answered the method's `confirm`, or the caller isn't a tap. */
  | { id: number; method: "call"; params: Target & { name: string; input?: unknown; confirmed?: boolean; caller?: Caller } }
  /** `subscription` is the caller's id, so a push can't arrive before the caller
   *  knows it; subscribing again under an id replaces it. */
  | { id: number; method: "subscribe"; params: Target & { subscription: string; name: string; input?: unknown } }
  | { id: number; method: "unsubscribe"; params: { subscription: string } }
  | { id: number; method: "reload"; params: Target }
  /**
   * The folder's whole set of schedules, replacing the last. `plugins` are the
   * ones they name: a folder opened only for them imports nothing else.
   */
  | { id: number; method: "crons"; params: { dir: string; plugins: string[]; crons: CronEntry[] } };

/** `stream`: the method is a stream; its result starts empty and pushes add `items`. */
export type Subscribed = { subscription: string; revision: number; result: unknown; stream?: true };

export type Reply = { id: number; result: unknown } | { id: number; error: CallError };

/**
 * A subscription's news. A query's new result (or error), sent only when it
 * differs from the last one sent. A stream's new `items`, to add to what it
 * holds, with `done` once it ends (and `error` when it failed); a stream's
 * `result` replaces everything held, as when its plugins reload and it starts over.
 */
export type DataPush = {
  method: "data";
  params: { subscription: string; revision: number } & (
    | { result: unknown }
    | { error: CallError }
    | { items: unknown[]; done?: true; error?: CallError }
  );
};

/** A plugin's alert for the phones, for the host to push. */
export type NotifyPush = { method: "notify"; params: SentNotice };

/**
 * A plugin's `ctx.host`: a request the server makes of the host, under the
 * server's own ids, which the host answers with a `Reply`.
 */
export type HostCall = { id: number; method: "host"; params: { plugin: string; method: string; params: unknown } };

// Between the server and a project's Worker.

export type ToWorker =
  /** `only` names the plugins to import; null is all of them. */
  | { type: "open"; dir: string; dataDir: string; root: string; only: string[] | null }
  /** The folder's schedules; the reply is each one's status. */
  | { type: "crons"; id: number; crons: CronEntry[] }
  | { type: "list"; id: number }
  | { type: "call"; id: number; name: string; input: unknown; confirmed: boolean; caller: Caller; root?: string }
  | { type: "subscribe"; id: number; subscription: string; name: string; input: unknown; root?: string }
  | { type: "unsubscribe"; subscription: string }
  /** Another Worker wrote these tags. */
  | { type: "invalidate"; tags: string[] }
  /** The host's answer to a `host` the Worker sent. */
  | { type: "hosted"; id: number; result: unknown }
  | { type: "hosted"; id: number; error: CallError }
  /** The Worker is about to end: abort its streams, so what they started stops too. */
  | { type: "stop" };

export type FromWorker =
  | { type: "reply"; id: number; result: unknown; stream?: true }
  | { type: "reply"; id: number; error: CallError }
  | { type: "data"; subscription: string; result: unknown }
  | { type: "data"; subscription: string; error: CallError }
  /** This Worker wrote these tags; the other Workers re-run what read them. */
  | { type: "invalidated"; tags: string[] }
  /** A plugin here sent an alert. */
  | { type: "notified"; notice: SentNotice }
  /** A plugin here called a host method; the answer comes back as `hosted`. */
  | { type: "host"; id: number; plugin: string; method: string; params: unknown }
  /** A stream's items since the last, or its end. */
  | { type: "items"; subscription: string; items: unknown[]; done?: true; error?: CallError }
  /** Its streams are aborted, after a `stop`. */
  | { type: "stopped" };

export type { Manifest };
