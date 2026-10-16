import { toCallError, type CallError } from "./errors";
import type { FromWorker, ToWorker } from "./protocol";
import { Runtime } from "./runtime";

// One project checkout's plugins, in a Worker of the plugins server: its own module
// cache, so reloading the code is ending this Worker and starting another.
// It keeps the subscriptions' queries and re-runs those a write touched.

declare var self: Worker;

/** How long writes gather before the queries they touched re-run, so a burst is one re-run. */
const settleMs = Number(process.env.REPOGO_PLUGINS_SETTLE_MS ?? 100);

type Subscription = {
  name: string;
  input: unknown;
  reads: Set<string>;
  /** Bumped per run, so a slow run that finishes after a newer one is dropped. */
  run: number;
};

let runtime: Runtime | null = null;
/** Requests that arrive while the plugins load wait for it. */
let ready: Promise<void> = Promise.resolve();
/** Set when the plugins failed to load; every request answers with it until a reload. */
let loadError: CallError | null = null;
const subscriptions = new Map<string, Subscription>();
/** Subscribes whose first run is still out; an unsubscribe meanwhile removes
 *  the key, and the run then registers nothing. */
const starting = new Set<string>();
const touched = new Set<string>();
let settling: Timer | null = null;

function post(message: FromWorker) {
  self.postMessage(message);
}

self.onmessage = async (event: MessageEvent<ToWorker>) => {
  const message = event.data;
  if (message.type === "open") {
    ready = open(message.dir, message.dataDir, message.root);
    return;
  }
  await ready;
  if (message.type === "invalidate") return settle(message.tags);
  if (message.type === "unsubscribe") {
    starting.delete(message.subscription);
    subscriptions.delete(message.subscription);
    return;
  }
  if (!runtime || loadError) {
    return post({ type: "reply", id: message.id, error: loadError ?? { code: "INTERNAL", message: "The plugins did not open." } });
  }
  if (message.type === "list") return post({ type: "reply", id: message.id, result: runtime.manifest() });
  if (message.type === "call") return call(message.id, message.name, message.input, message.confirmed);
  if (message.type === "subscribe") return subscribe(message.id, message.subscription, message.name, message.input);
};

async function open(dir: string, dataDir: string, root: string) {
  try {
    runtime = await Runtime.open({ dataDir, root });
    // A write here re-runs this checkout's queries, and is passed on for the others.
    runtime.onInvalidate((tags) => {
      post({ type: "invalidated", tags });
      settle(tags);
    });
    await runtime.loadDir(dir);
  } catch (error) {
    loadError = { code: "INTERNAL", message: `The plugins in ${dir} failed to load: ${toCallError(error).message}` };
  }
}

async function call(id: number, name: string, input: unknown, confirmed: boolean) {
  try {
    const { result } = await runtime!.call(name, input, { confirmed });
    post({ type: "reply", id, result });
  } catch (error) {
    post({ type: "reply", id, error: toCallError(error) });
  }
}

// The first run answers the request; a failing first run registers nothing.
async function subscribe(id: number, key: string, name: string, input: unknown) {
  starting.add(key);
  try {
    const { result, reads } = await runtime!.call(name, input, { expect: "query" });
    if (starting.delete(key)) subscriptions.set(key, { name, input, reads: new Set(reads), run: 0 });
    post({ type: "reply", id, result });
  } catch (error) {
    starting.delete(key);
    post({ type: "reply", id, error: toCallError(error) });
  }
}

function settle(tags: string[]) {
  for (const tag of tags) touched.add(tag);
  settling ??= setTimeout(rerun, settleMs);
}

function rerun() {
  settling = null;
  const tags = [...touched];
  touched.clear();
  for (const [key, subscription] of subscriptions) {
    if (tags.some((tag) => subscription.reads.has(tag))) void refresh(key, subscription);
  }
}

async function refresh(key: string, subscription: Subscription) {
  const run = ++subscription.run;
  const answer = await runtime!
    .call(subscription.name, subscription.input, { expect: "query" })
    .then(({ result, reads }) => ({ result, reads }), (error) => ({ error: toCallError(error) }));
  // Unsubscribed meanwhile, or a newer run started: this answer is stale.
  if (subscriptions.get(key) !== subscription || subscription.run !== run) return;
  if ("error" in answer) return post({ type: "data", subscription: key, error: answer.error });
  subscription.reads = new Set(answer.reads);
  post({ type: "data", subscription: key, result: answer.result });
}
