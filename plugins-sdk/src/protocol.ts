import type { CallError } from "./errors";
import type { Manifest } from "./runtime";

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
  | { id: number; method: "call"; params: Target & { name: string; input?: unknown; confirmed?: boolean } }
  /** `subscription` is the caller's id, so a push can't arrive before the caller
   *  knows it; subscribing again under an id replaces it. */
  | { id: number; method: "subscribe"; params: Target & { subscription: string; name: string; input?: unknown } }
  | { id: number; method: "unsubscribe"; params: { subscription: string } }
  | { id: number; method: "reload"; params: Target };

export type Subscribed = { subscription: string; revision: number; result: unknown };

export type Reply = { id: number; result: unknown } | { id: number; error: CallError };

/** A subscription's new result, sent only when it differs from the last one sent. */
export type DataPush = {
  method: "data";
  params: { subscription: string; revision: number } & ({ result: unknown } | { error: CallError });
};

// Between the server and a project's Worker.

export type ToWorker =
  | { type: "open"; dir: string; dataDir: string; root: string }
  | { type: "list"; id: number }
  | { type: "call"; id: number; name: string; input: unknown; confirmed: boolean; root?: string }
  | { type: "subscribe"; id: number; subscription: string; name: string; input: unknown; root?: string }
  | { type: "unsubscribe"; subscription: string }
  /** Another Worker wrote these tags. */
  | { type: "invalidate"; tags: string[] };

export type FromWorker =
  | { type: "reply"; id: number; result: unknown }
  | { type: "reply"; id: number; error: CallError }
  | { type: "data"; subscription: string; result: unknown }
  | { type: "data"; subscription: string; error: CallError }
  /** This Worker wrote these tags; the other Workers re-run what read them. */
  | { type: "invalidated"; tags: string[] };

export type { Manifest };
