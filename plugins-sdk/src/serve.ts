import type { Socket } from "bun";
import { chmod, mkdir, rm } from "node:fs/promises";
import { dirname, isAbsolute, resolve } from "node:path";
import { z } from "zod";
import { toCallError, type CallError } from "./errors";
import type { DataPush, FromWorker, Reply, Request, ToWorker } from "./protocol";

// The plugins server: one process for every project on this machine, listening on
// a Unix socket the host connects to. Each project checkout's plugins run in their
// own Worker; this thread routes requests to them and keeps the subscriptions,
// sending a subscription's result only when it changed.

export type ServeOptions = {
  /** The socket's path; created with mode 0600. */
  socket: string;
  /** Where the databases go, `<dataDir>/<db>.db`. */
  dataDir: string;
};

const pluginsDir = z.string().refine(isAbsolute, "dir is an absolute path");
const target = z.object({ dir: pluginsDir });
const named = target.extend({ name: z.string().min(1), input: z.unknown().optional() });

const requestSchema = z.discriminatedUnion("method", [
  z.object({ id: z.number(), method: z.literal("list"), params: target }),
  z.object({ id: z.number(), method: z.literal("call"), params: named.extend({ confirmed: z.boolean().optional() }) }),
  z.object({ id: z.number(), method: z.literal("subscribe"), params: named.extend({ subscription: z.string().min(1) }) }),
  z.object({ id: z.number(), method: z.literal("unsubscribe"), params: z.object({ subscription: z.string() }) }),
  z.object({ id: z.number(), method: z.literal("reload"), params: target }),
]);

type Connection = Socket<{ inbox: string; outbox: Uint8Array[] }>;

type Subscription = {
  connection: Connection;
  checkout: string;
  name: string;
  input: unknown;
  revision: number;
  /** Of the last result or error sent, so an unchanged re-run sends nothing. */
  hash: string;
  /** The Worker's id for it: new on each subscribe and restart, so an answer
   *  or push from one it replaced can't reach it. */
  key: string;
};

/** A subscribe waiting on its first answer. */
type Pending = { connection: Connection; checkout: string; key: string };

type Answer = { result: unknown } | { error: CallError };

/** One checkout's plugins in a Worker: requests out, replies and pushes back. */
class Checkout {
  private worker!: Worker;
  private waiting = new Map<number, (answer: Answer) => void>();
  private nextID = 0;
  private stopping = false;

  constructor(
    readonly dir: string,
    private dataDir: string,
    private onMessage: (checkout: Checkout, message: FromWorker) => void,
    private onCrash: (checkout: Checkout) => void,
  ) {
    this.start();
  }

  private start() {
    // Beside this file: worker.ts in the source, worker.js in the host's bundle.
    this.worker = new Worker(new URL(import.meta.url.endsWith(".ts") ? "./worker.ts" : "./worker.js", import.meta.url));
    this.worker.onmessage = (event: MessageEvent<FromWorker>) => {
      const message = event.data;
      if (message.type !== "reply") return this.onMessage(this, message);
      const resolve = this.waiting.get(message.id);
      this.waiting.delete(message.id);
      resolve?.("error" in message ? { error: message.error } : { result: message.result });
    };
    this.worker.addEventListener("close", () => {
      this.failWaiting("The project's plugins stopped.");
      if (!this.stopping) this.onCrash(this);
    });
    // dir is `<checkout>/.repogo/plugins`, so the checkout is two folders up.
    this.post({ type: "open", dir: this.dir, dataDir: this.dataDir, root: resolve(this.dir, "..", "..") });
  }

  request(message: { type: "list" } | { type: "call"; name: string; input: unknown; confirmed: boolean } | { type: "subscribe"; subscription: string; name: string; input: unknown }): Promise<Answer> {
    const id = ++this.nextID;
    return new Promise((resolve) => {
      this.waiting.set(id, resolve);
      this.post({ ...message, id } as ToWorker);
    });
  }

  post(message: ToWorker) {
    this.worker.postMessage(message);
  }

  stop() {
    this.stopping = true;
    this.failWaiting("The project's plugins reloaded.");
    this.worker.terminate();
  }

  private failWaiting(message: string) {
    for (const resolve of this.waiting.values()) resolve({ error: { code: "INTERNAL", message } });
    this.waiting.clear();
  }
}

export async function serve(options: ServeOptions) {
  const checkouts = new Map<string, Checkout>();
  const subscriptions = new Map<string, Subscription>();
  const pending = new Map<string, Pending>();
  /** Worker key → subscription id, for the Worker's pushes. */
  const byKey = new Map<string, string>();
  let lastKey = 0;
  const newKey = () => `w${++lastKey}`;

  function checkoutFor(dir: string) {
    let checkout = checkouts.get(dir);
    if (!checkout) {
      checkout = new Checkout(dir, options.dataDir, fromWorker, restart);
      checkouts.set(dir, checkout);
    }
    return checkout;
  }

  function fromWorker(from: Checkout, message: FromWorker) {
    if (message.type === "data") {
      const id = byKey.get(message.subscription);
      if (id) deliver(id, "error" in message ? { error: message.error } : { result: message.result });
      return;
    }
    if (message.type === "invalidated") {
      // Any other Worker may read the same database: a shared db, or a worktree.
      for (const checkout of checkouts.values()) {
        if (checkout !== from) checkout.post({ type: "invalidate", tags: message.tags });
      }
    }
  }

  function deliver(id: string, answer: Answer) {
    const subscription = subscriptions.get(id);
    if (!subscription) return;
    const hash = hashOf(answer);
    if (hash === subscription.hash) return;
    subscription.hash = hash;
    subscription.revision += 1;
    const push: DataPush = { method: "data", params: { subscription: id, revision: subscription.revision, ...answer } };
    send(subscription.connection, push);
  }

  // A new Worker for the checkout, with its subscriptions taken up again: their
  // results go out as pushes if the new code answers differently.
  function restart(old: Checkout) {
    if (checkouts.get(old.dir) !== old) return;
    old.stop();
    const checkout = new Checkout(old.dir, options.dataDir, fromWorker, restart);
    checkouts.set(old.dir, checkout);
    for (const [id, subscription] of subscriptions) {
      if (subscription.checkout !== old.dir) continue;
      byKey.delete(subscription.key);
      const key = newKey();
      subscription.key = key;
      byKey.set(key, id);
      void checkout.request({ type: "subscribe", subscription: key, name: subscription.name, input: subscription.input }).then((answer) => {
        // Ended or replaced while the new Worker ran it: drop what it registered.
        if (subscriptions.get(id)?.key === key) deliver(id, answer);
        else checkout.post({ type: "unsubscribe", subscription: key });
      });
    }
  }

  async function handle(connection: Connection, request: Request): Promise<Reply> {
    const { id } = request;
    switch (request.method) {
      case "list":
      case "call": {
        const checkout = checkoutFor(request.params.dir);
        const answer =
          request.method === "list"
            ? await checkout.request({ type: "list" })
            : await checkout.request({
              type: "call", name: request.params.name, input: request.params.input ?? {},
              confirmed: request.params.confirmed ?? false,
            });
        return { id, ...answer };
      }
      case "subscribe": {
        const { dir, name, subscription } = request.params;
        const input = request.params.input ?? {};
        // Subscribing again under an id replaces it.
        unsubscribe(subscription);
        const checkout = checkoutFor(dir);
        const key = newKey();
        pending.set(subscription, { connection, checkout: checkout.dir, key });
        const answer = await checkout.request({ type: "subscribe", subscription: key, name, input });
        if (pending.get(subscription)?.key !== key) {
          // Ended or replaced while its first run was out: it never starts.
          checkout.post({ type: "unsubscribe", subscription: key });
          return { id, error: { code: "INTERNAL", message: `${subscription} ended before its first answer.` } };
        }
        pending.delete(subscription);
        if ("error" in answer) return { id, ...answer };
        subscriptions.set(subscription, { connection, checkout: checkout.dir, name, input, revision: 1, hash: hashOf(answer), key });
        byKey.set(key, subscription);
        return { id, result: { subscription, revision: 1, result: answer.result } };
      }
      case "unsubscribe": {
        unsubscribe(request.params.subscription);
        return { id, result: {} };
      }
      case "reload": {
        const checkout = checkouts.get(request.params.dir);
        if (checkout) restart(checkout);
        return { id, result: {} };
      }
    }
  }

  /** Ends a subscription, or one still waiting on its first answer. */
  function unsubscribe(id: string) {
    const waiting = pending.get(id);
    if (waiting) {
      pending.delete(id);
      checkouts.get(waiting.checkout)?.post({ type: "unsubscribe", subscription: waiting.key });
    }
    const subscription = subscriptions.get(id);
    if (subscription) {
      subscriptions.delete(id);
      byKey.delete(subscription.key);
      checkouts.get(subscription.checkout)?.post({ type: "unsubscribe", subscription: subscription.key });
    }
  }

  async function receive(connection: Connection, line: string) {
    let raw: unknown;
    try {
      raw = JSON.parse(line);
    } catch {
      return send(connection, { id: -1, error: { code: "VALIDATION", message: "Each line is one JSON request." } });
    }
    const parsed = requestSchema.safeParse(raw);
    if (!parsed.success) {
      const id = typeof (raw as { id?: unknown })?.id === "number" ? (raw as { id: number }).id : -1;
      return send(connection, { id, error: toCallError(parsed.error) });
    }
    send(connection, await handle(connection, parsed.data as Request));
  }

  await mkdir(dirname(options.socket), { recursive: true });
  await rm(options.socket, { force: true });
  const listener = Bun.listen<{ inbox: string; outbox: Uint8Array[] }>({
    unix: options.socket,
    socket: {
      open(connection) {
        connection.data = { inbox: "", outbox: [] };
      },
      data(connection, chunk) {
        connection.data.inbox += chunk.toString();
        let newline: number;
        while ((newline = connection.data.inbox.indexOf("\n")) >= 0) {
          const line = connection.data.inbox.slice(0, newline).trim();
          connection.data.inbox = connection.data.inbox.slice(newline + 1);
          if (line) void receive(connection, line);
        }
      },
      drain(connection) {
        flush(connection);
      },
      close(connection) {
        // A host that goes away takes its subscriptions with it.
        for (const [id, subscription] of subscriptions) {
          if (subscription.connection === connection) unsubscribe(id);
        }
        for (const [id, waiting] of pending) {
          if (waiting.connection === connection) unsubscribe(id);
        }
      },
    },
  });
  await chmod(options.socket, 0o600);

  return {
    stop() {
      listener.stop(true);
      for (const checkout of checkouts.values()) checkout.stop();
      checkouts.clear();
      subscriptions.clear();
      void rm(options.socket, { force: true });
    },
  };
}

function hashOf(answer: Answer) {
  return Bun.hash(JSON.stringify(answer)).toString(36);
}

const encoder = new TextEncoder();

// A write the socket can't take at once waits in the outbox for `drain`.
function send(connection: Connection, message: Reply | DataPush) {
  connection.data.outbox.push(encoder.encode(JSON.stringify(message) + "\n"));
  flush(connection);
}

function flush(connection: Connection) {
  const outbox = connection.data.outbox;
  while (outbox.length > 0) {
    const chunk = outbox[0]!;
    const written = connection.write(chunk);
    if (written < chunk.length) {
      outbox[0] = chunk.subarray(Math.max(written, 0));
      return;
    }
    outbox.shift();
  }
}
