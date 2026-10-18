import "./self";
import { MethodError, toCallError, type CallError } from "./errors";
import type { FromWorker, ToWorker } from "./protocol";
import { Runtime } from "./runtime";

// One project checkout's plugins, in a Worker of the plugins server: its own module
// cache, so reloading the code is ending this Worker and starting another.
// It keeps the subscriptions' queries and re-runs those a write touched, and
// runs the subscriptions' streams, sending their items in batches, and the
// plugins' crons when they come due.

declare var self: Worker;

/** How long writes gather before the queries they touched re-run, so a burst is one re-run. */
const settleMs = Number(process.env.REPOGO_PLUGINS_SETTLE_MS ?? 100);

/** The most items one batch carries; a faster stream loses its oldest. */
const batchLimit = 500;

type Stream = {
  abort: AbortController;
  /** Settles once the plugin's code has returned, after what it started has exited. */
  ended: Promise<unknown>;
  /** Items since the last batch went out. */
  items: unknown[];
  sending: Timer | null;
};

type Subscription = {
  name: string;
  input: unknown;
  root?: string;
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
const streams = new Map<string, Stream>();
const touched = new Set<string>();
let settling: Timer | null = null;
/** Starts the plugins' crons as they come due; one tick a second, seconds being a cron's finest field. */
let cronTicker: Timer | null = null;
/** Expires kv keys, trims streams and rescans buckets. */
let storageTicker: Timer | null = null;
const storageTickMs = 5_000;

/** The plugins' `ctx.host` calls out to the host, by id, until it answers. */
const hostCalls = new Map<number, { resolve: (result: unknown) => void; reject: (error: MethodError) => void }>();
let lastHostCall = 0;

function post(message: FromWorker) {
  self.postMessage(message);
}

function host(plugin: string, method: string, params: unknown) {
  const id = ++lastHostCall;
  return new Promise((resolve, reject) => {
    hostCalls.set(id, { resolve, reject });
    post({ type: "host", id, plugin, method, params });
  });
}

self.onmessage = async (event: MessageEvent<ToWorker>) => {
  const message = event.data;
  if (message.type === "open") {
    ready = open(message.dir, message.dataDir, message.root);
    return;
  }
  if (message.type === "hosted") {
    const waiting = hostCalls.get(message.id);
    hostCalls.delete(message.id);
    if ("error" in message) waiting?.reject(new MethodError(message.error.code, message.error.message, message.error.fieldErrors));
    else waiting?.resolve(message.result);
    return;
  }
  await ready;
  if (message.type === "invalidate") return settle(message.tags);
  if (message.type === "unsubscribe") {
    starting.delete(message.subscription);
    subscriptions.delete(message.subscription);
    endStream(message.subscription);
    return;
  }
  if (message.type === "stop") {
    if (cronTicker) clearTimeout(cronTicker);
    if (storageTicker) clearInterval(storageTicker);
    cronTicker = null;
    storageTicker = null;
    const ending = [...streams.values()].map((stream) => stream.ended);
    for (const key of [...streams.keys()]) endStream(key);
    // The plugins' code gets to see its children exit, so none is left unreaped.
    await Promise.allSettled(ending);
    runtime?.close();
    return post({ type: "stopped" });
  }
  if (!runtime || loadError) {
    return post({ type: "reply", id: message.id, error: loadError ?? { code: "INTERNAL", message: "The plugins did not open." } });
  }
  if (message.type === "list") return post({ type: "reply", id: message.id, result: runtime.manifest() });
  if (message.type === "call") return call(message.id, message.name, message.input, message.confirmed, message.root);
  if (message.type === "subscribe") {
    if (runtime.methods.get(message.name)?.kind === "stream") {
      return subscribeStream(message.id, message.subscription, message.name, message.input, message.root);
    }
    return subscribe(message.id, message.subscription, message.name, message.input, message.root);
  }
};

// Each tick lands just after a whole second, the finest a cron names, so a run starts within milliseconds of its time.
function tickOnTheSecond() {
  cronTicker = setTimeout(() => {
    runtime?.tickCrons();
    tickOnTheSecond();
  }, 1000 - (Date.now() % 1000) + 2);
}

async function open(dir: string, dataDir: string, root: string) {
  try {
    runtime = await Runtime.open({ dataDir, root, host });
    // A write here re-runs this checkout's queries, and is passed on for the others.
    runtime.onInvalidate((tags) => {
      post({ type: "invalidated", tags });
      settle(tags);
    });
    runtime.onNotify((notice) => post({ type: "notified", notice }));
    await runtime.loadDir(dir);
    if (runtime.crons.length > 0) tickOnTheSecond();
    if (runtime.hasStorage) storageTicker = setInterval(() => void runtime?.tickStorage(), storageTickMs);
  } catch (error) {
    loadError = { code: "INTERNAL", message: `The plugins in ${dir} failed to load: ${toCallError(error).message}` };
  }
}

async function call(id: number, name: string, input: unknown, confirmed: boolean, root?: string) {
  try {
    const { result } = await runtime!.call(name, input, { confirmed, root });
    post({ type: "reply", id, result });
  } catch (error) {
    post({ type: "reply", id, error: toCallError(error) });
  }
}

// The first run answers the request; a failing first run registers nothing.
async function subscribe(id: number, key: string, name: string, input: unknown, root?: string) {
  starting.add(key);
  try {
    const { result, reads } = await runtime!.call(name, input, { expect: "query", root });
    if (starting.delete(key)) subscriptions.set(key, { name, input, root, reads: new Set(reads), run: 0 });
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
    .call(subscription.name, subscription.input, { expect: "query", root: subscription.root })
    .then(({ result, reads }) => ({ result, reads }), (error) => ({ error: toCallError(error) }));
  // Unsubscribed meanwhile, or a newer run started: this answer is stale.
  if (subscriptions.get(key) !== subscription || subscription.run !== run) return;
  if ("error" in answer) return post({ type: "data", subscription: key, error: answer.error });
  subscription.reads = new Set(answer.reads);
  post({ type: "data", subscription: key, result: answer.result });
}

// A stream starts empty and answers at once; its items follow as batches, one
// per settle window, so a chatty source is a push every 100 ms, not per line.
function subscribeStream(id: number, key: string, name: string, input: unknown, root?: string) {
  const stream: Stream = { abort: new AbortController(), ended: Promise.resolve(), items: [], sending: null };
  try {
    stream.ended = runtime!.stream(name, input, { root, signal: stream.abort.signal }, (item) => {
      stream.items.push(item);
      if (stream.items.length > batchLimit) stream.items.shift();
      stream.sending ??= setTimeout(() => send(key, stream), settleMs);
    });
  } catch (error) {
    return post({ type: "reply", id, error: toCallError(error) });
  }
  streams.set(key, stream);
  post({ type: "reply", id, result: [], stream: true });
  stream.ended.then(
    () => send(key, stream, {}),
    (error) => send(key, stream, { error: toCallError(error) }),
  );
}

/** Sends what the stream gathered; `end` also says it's over, and why when it failed. */
function send(key: string, stream: Stream, end?: { error?: CallError }) {
  if (stream.sending) clearTimeout(stream.sending);
  stream.sending = null;
  // Ended by an unsubscribe or a stop: nobody is listening.
  if (streams.get(key) !== stream) return;
  const items = stream.items;
  stream.items = [];
  if (!end) return post({ type: "items", subscription: key, items });
  streams.delete(key);
  post({ type: "items", subscription: key, items, done: true, ...(end.error ? { error: end.error } : {}) });
}

function endStream(key: string) {
  const stream = streams.get(key);
  if (!stream) return;
  streams.delete(key);
  if (stream.sending) clearTimeout(stream.sending);
  stream.abort.abort();
}
