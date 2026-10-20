import type { Socket } from "bun";
import { chmod, mkdir, rm } from "node:fs/promises";
import { dirname, isAbsolute, resolve } from "node:path";
import { z } from "zod";
import type { Caller, CronEntry, CronStatus } from "./context";
import { toCallError, type CallError } from "./errors";
import type { DataPush, FromWorker, HostCall, LogPush, NotifyPush, Reply, Request, ToWorker } from "./protocol";
import type { UploadedFile } from "./uploads";

// The plugins server: one process for every project on this machine, listening on
// a Unix socket the host connects to. Each project checkout's plugins run in their
// own Worker; this thread routes requests to them and keeps the subscriptions,
// sending a query's result only when it changed, and a stream's items as they come.

export type ServeOptions = {
  /** The socket's path; created with mode 0600. */
  socket: string;
  /** Where each namespace's folder goes, `<dataDir>/<namespace>/`. */
  dataDir: string;
};

const pluginsDir = z.string().refine(isAbsolute, "dir is an absolute path");
const target = z.object({ dir: pluginsDir, root: z.string().refine(isAbsolute, "root is an absolute path").optional() });
const named = target.extend({ name: z.string().min(1), input: z.unknown().optional() });
const uploadedFile = z.object({ id: z.string(), name: z.string(), type: z.string(), size: z.number(), path: z.string() });

const requestSchema = z.discriminatedUnion("method", [
  z.object({ id: z.number(), method: z.literal("list"), params: target }),
  z.object({ id: z.number(), method: z.literal("call"), params: named.extend({ files: z.array(uploadedFile).optional(), confirmed: z.boolean().optional(), caller: z.enum(["card", "agent", "cron", "code", "http"]).optional(), chat: z.string().optional() }) }),
  z.object({ id: z.number(), method: z.literal("subscribe"), params: named.extend({ subscription: z.string().min(1) }) }),
  z.object({ id: z.number(), method: z.literal("unsubscribe"), params: z.object({ subscription: z.string() }) }),
  z.object({ id: z.number(), method: z.literal("reload"), params: target }),
  z.object({
    id: z.number(), method: z.literal("crons"),
    params: z.object({
      dir: pluginsDir,
      plugins: z.array(z.string().min(1)),
      crons: z.array(z.object({ plugin: z.string().min(1), method: z.string().min(1), schedule: z.string().min(1), timezone: z.string().optional() })),
    }),
  }),
]);

type Connection = Socket<{ inbox: string; outbox: Uint8Array[] }>;

type Subscription = {
  connection: Connection;
  checkout: string;
  name: string;
  input: unknown;
  root?: string;
  revision: number;
  /** A stream's pushes add items, so each one goes out; a query's go only when changed. */
  stream: boolean;
  /** Of the last result or error sent, so an unchanged re-run sends nothing. */
  hash: string;
  /** The Worker's id for it: new on each subscribe and restart, so an answer
   *  or push from one it replaced can't reach it. */
  key: string;
};

/** A subscribe waiting on its first answer. */
type Pending = { connection: Connection; checkout: string; key: string };

/** A plugin's `ctx.host` sent up to the host: the Worker's id for it, to answer under. */
type HostWait = { connection: Connection; checkout: Checkout; id: number };

type Answer = { result: unknown; stream?: true } | { error: CallError };

/** How long a stopping Worker gets to abort its streams before it is ended. */
const stopGraceMs = 1000;

/**
 * One checkout's plugins in a Worker: requests out, replies and pushes back.
 * `only` and `crons` outlive the Worker, so one started again picks them up.
 */
class Checkout {
  private worker!: Worker;
  private waiting = new Map<number, (answer: Answer) => void>();
  private nextID = 0;
  private stopping = false;
  private stopped: (() => void) | null = null;

  constructor(
    readonly dir: string,
    private dataDir: string,
    private onMessage: (checkout: Checkout, message: FromWorker) => void,
    private onCrash: (checkout: Checkout) => void,
    /** The plugins it imports, for a folder opened only for its schedules; null is all of them. */
    readonly only: string[] | null = null,
    /** The schedules the host set for this folder. */
    public crons: CronEntry[] = [],
  ) {
    this.start();
  }

  private start() {
    // Beside this file: worker.ts in the source, worker.js in the host's bundle.
    this.worker = new Worker(new URL(import.meta.url.endsWith(".ts") ? "./worker.ts" : "./worker.js", import.meta.url));
    this.worker.onmessage = (event: MessageEvent<FromWorker>) => {
      const message = event.data;
      if (message.type === "stopped") return this.stopped?.();
      if (message.type !== "reply") return this.onMessage(this, message);
      const resolve = this.waiting.get(message.id);
      this.waiting.delete(message.id);
      resolve?.("error" in message ? { error: message.error } : { result: message.result, ...(message.stream ? { stream: true as const } : {}) });
    };
    this.worker.addEventListener("close", () => {
      this.failWaiting("The project's plugins stopped.");
      if (!this.stopping) this.onCrash(this);
    });
    // dir is `<checkout>/.repogo/plugins`, so the checkout is two folders up.
    this.post({ type: "open", dir: this.dir, dataDir: this.dataDir, root: resolve(this.dir, "..", ".."), only: this.only });
    if (this.crons.length) void this.setCrons(this.crons);
  }

  /** Gives the Worker its schedules, the whole set, and answers each one's status. */
  async setCrons(crons: CronEntry[]): Promise<{ statuses: CronStatus[] } | { error: CallError }> {
    this.crons = crons;
    const answer = await this.request({ type: "crons", crons });
    return "error" in answer ? answer : { statuses: answer.result as CronStatus[] };
  }

  request(message: { type: "list" } | { type: "crons"; crons: CronEntry[] } | { type: "call"; name: string; input: unknown; files?: UploadedFile[]; confirmed: boolean; caller: Caller; root?: string; chat?: string } | { type: "subscribe"; subscription: string; name: string; input: unknown; root?: string }): Promise<Answer> {
    const id = ++this.nextID;
    return new Promise((resolve) => {
      this.waiting.set(id, resolve);
      this.post({ ...message, id } as ToWorker);
    });
  }

  post(message: ToWorker) {
    this.worker.postMessage(message);
  }

  /** Ends the Worker once its streams have aborted, so a process one started
   *  doesn't outlive it; one that doesn't answer is ended anyway. */
  async stop() {
    this.stopping = true;
    this.failWaiting("The project's plugins reloaded.");
    await new Promise<void>((resolve) => {
      this.stopped = resolve;
      setTimeout(resolve, stopGraceMs);
      this.post({ type: "stop" });
    });
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
  /** The hosts connected, for pushes no subscription addresses. */
  const connections = new Set<Connection>();
  /** Worker key → subscription id, for the Worker's pushes. */
  const byKey = new Map<string, string>();
  /** Host calls out, by the id this server sent them under. */
  const hostCalls = new Map<number, HostWait>();
  let lastHostCall = 0;
  let lastKey = 0;
  const newKey = () => `w${++lastKey}`;

  // What a card or agent asks for gets the whole folder: one opened only for
  // its schedules starts again with every plugin, keeping the schedules.
  function checkoutFor(dir: string) {
    const checkout = checkouts.get(dir);
    if (!checkout) {
      const opened = new Checkout(dir, options.dataDir, fromWorker, restart);
      checkouts.set(dir, opened);
      return opened;
    }
    return checkout.only ? restart(checkout, null) : checkout;
  }

  /**
   * Sets a folder's schedules, the whole set. A folder no one else opened
   * imports only the plugins they name, and closes when its last one goes.
   */
  async function setCrons(dir: string, plugins: string[], crons: CronEntry[]) {
    const held = checkouts.get(dir);
    if (!crons.length) {
      if (!held) return { statuses: [] };
      if (held.only) {
        checkouts.delete(dir);
        await held.stop();
        return { statuses: [] };
      }
      return held.setCrons([]);
    }
    if (!held) {
      const opened = new Checkout(dir, options.dataDir, fromWorker, restart, plugins);
      checkouts.set(dir, opened);
      return opened.setCrons(crons);
    }
    // A folder opened only for schedules imports exactly the plugins they name:
    // one added or one dropped opens it again, so a dropped one's code can't come back.
    if (!held.only || sameSet(held.only, plugins)) return held.setCrons(crons);
    held.crons = crons;
    const reopened = restart(held, plugins);
    return reopened.setCrons(crons);
  }

  function fromWorker(from: Checkout, message: FromWorker) {
    if (message.type === "data") {
      const id = byKey.get(message.subscription);
      if (id) deliver(id, "error" in message ? { error: message.error } : { result: message.result });
      return;
    }
    if (message.type === "items") {
      const id = byKey.get(message.subscription);
      const subscription = id ? subscriptions.get(id) : undefined;
      if (!id || !subscription) return;
      subscription.revision += 1;
      const { items, done, error } = message;
      send(subscription.connection, {
        method: "data",
        params: { subscription: id, revision: subscription.revision, items, ...(done ? { done } : {}), ...(error ? { error } : {}) },
      });
      return;
    }
    if (message.type === "invalidated") {
      // Any other Worker may read the same database: a shared namespace, or a worktree.
      for (const checkout of checkouts.values()) {
        if (checkout !== from) checkout.post({ type: "invalidate", tags: message.tags });
      }
      return;
    }
    if (message.type === "notified") {
      const push: NotifyPush = { method: "notify", params: message.notice };
      for (const connection of connections) send(connection, push);
      return;
    }
    if (message.type === "log") {
      const push: LogPush = { method: "log", params: { dir: from.dir, records: message.records } };
      for (const connection of connections) send(connection, push);
      return;
    }
    if (message.type === "host") {
      // One host connects; the first one there answers.
      const [connection] = connections;
      if (!connection) {
        from.post({ type: "hosted", id: message.id, error: { code: "INTERNAL", message: "The RepoGo host isn't connected." } });
        return;
      }
      const id = ++lastHostCall;
      hostCalls.set(id, { connection, checkout: from, id: message.id });
      const call: HostCall = { id, method: "host", params: { plugin: message.plugin, method: message.method, params: message.params } };
      send(connection, call);
    }
  }

  /** The host's answer to a host call, back to the Worker that made it. */
  function hosted(reply: Reply) {
    const waiting = hostCalls.get(reply.id);
    if (!waiting) return;
    hostCalls.delete(reply.id);
    waiting.checkout.post({ type: "hosted", id: waiting.id, ...("error" in reply ? { error: reply.error } : { result: reply.result }) });
  }

  function deliver(id: string, answer: Answer) {
    const subscription = subscriptions.get(id);
    if (!subscription) return;
    const hash = hashOf(answer);
    if (!subscription.stream && hash === subscription.hash) return;
    subscription.hash = hash;
    subscription.revision += 1;
    const news = "error" in answer ? { error: answer.error } : { result: answer.result };
    const push: DataPush = { method: "data", params: { subscription: id, revision: subscription.revision, ...news } };
    send(subscription.connection, push);
  }

  // A new Worker for the checkout, with its schedules and subscriptions taken up
  // again: their results go out as pushes if the new code answers differently.
  // A stream starts over, and its empty result tells the device to drop what it held.
  function restart(old: Checkout, only: string[] | null = old.only): Checkout {
    const current = checkouts.get(old.dir);
    if (current !== old) return current ?? old;
    void old.stop();
    const checkout = new Checkout(old.dir, options.dataDir, fromWorker, restart, only, old.crons);
    checkouts.set(old.dir, checkout);
    for (const [id, subscription] of subscriptions) {
      if (subscription.checkout !== old.dir) continue;
      byKey.delete(subscription.key);
      const key = newKey();
      subscription.key = key;
      byKey.set(key, id);
      void checkout.request({ type: "subscribe", subscription: key, name: subscription.name, input: subscription.input, root: subscription.root }).then((answer) => {
        // Ended or replaced while the new Worker ran it: drop what it registered.
        if (subscriptions.get(id)?.key === key) deliver(id, answer);
        else checkout.post({ type: "unsubscribe", subscription: key });
      });
    }
    return checkout;
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
              type: "call", name: request.params.name, input: request.params.input ?? {}, files: request.params.files,
              confirmed: request.params.confirmed ?? false, caller: request.params.caller ?? "code", root: request.params.root, chat: request.params.chat,
            });
        return { id, ...answer };
      }
      case "subscribe": {
        const { dir, name, subscription, root } = request.params;
        const input = request.params.input ?? {};
        // Subscribing again under an id replaces it.
        unsubscribe(subscription);
        const checkout = checkoutFor(dir);
        const key = newKey();
        pending.set(subscription, { connection, checkout: checkout.dir, key });
        const answer = await checkout.request({ type: "subscribe", subscription: key, name, input, root });
        if (pending.get(subscription)?.key !== key) {
          // Ended or replaced while its first run was out: it never starts.
          checkout.post({ type: "unsubscribe", subscription: key });
          return { id, error: { code: "INTERNAL", message: `${subscription} ended before its first answer.` } };
        }
        pending.delete(subscription);
        if ("error" in answer) return { id, ...answer };
        const stream = "stream" in answer && answer.stream === true;
        subscriptions.set(subscription, { connection, checkout: checkout.dir, name, input, root, revision: 1, stream, hash: hashOf(answer), key });
        byKey.set(key, subscription);
        return { id, result: { subscription, revision: 1, result: answer.result, ...(stream ? { stream } : {}) } };
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
      case "crons": {
        const { dir, plugins, crons } = request.params;
        const answer = await setCrons(dir, plugins, crons);
        return "error" in answer ? { id, error: answer.error } : { id, result: answer.statuses };
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
    // A line without a method is the host answering a host call.
    if (typeof raw === "object" && raw !== null && !("method" in raw) && typeof (raw as Reply).id === "number") {
      return hosted(raw as Reply);
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
        connections.add(connection);
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
        connections.delete(connection);
        // A host that goes away takes its subscriptions with it.
        for (const [id, subscription] of subscriptions) {
          if (subscription.connection === connection) unsubscribe(id);
        }
        for (const [id, waiting] of pending) {
          if (waiting.connection === connection) unsubscribe(id);
        }
        for (const [id, waiting] of hostCalls) {
          if (waiting.connection === connection) hosted({ id, error: { code: "INTERNAL", message: "The RepoGo host went away." } });
        }
      },
    },
  });
  await chmod(options.socket, 0o600);

  return {
    async stop() {
      listener.stop(true);
      const stopping = [...checkouts.values()].map((checkout) => checkout.stop());
      checkouts.clear();
      await Promise.all(stopping);
      subscriptions.clear();
      void rm(options.socket, { force: true });
    },
  };
}

function sameSet(a: string[], b: string[]) {
  return a.length === b.length && a.every((item) => b.includes(item));
}

function hashOf(answer: Answer) {
  return Bun.hash(JSON.stringify(answer)).toString(36);
}

const encoder = new TextEncoder();

// A write the socket can't take at once waits in the outbox for `drain`.
function send(connection: Connection, message: Reply | DataPush | NotifyPush | LogPush | HostCall) {
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
