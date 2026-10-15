import { afterEach, expect, test } from "bun:test";
import { mkdir, mkdtemp, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import type { DataPush, Reply } from "../src/protocol";
import { serve } from "../src/serve";

// Re-runs settle fast here; the server's default is 100 ms.
process.env.REPOGO_PLUGINS_SETTLE_MS = "10";

const sdk = resolve(import.meta.dir, "..");

const notesApp = `import { definePlugin, method, z } from "@repogo/plugins";
export default definePlugin({
  collections: {
    notes: z.object({ text: z.string() }),
    tags: z.object({ name: z.string() }),
  },
  methods: {
    texts: method(z.object({}), async (_, ctx) => (await ctx.db.notes.list({ order: "asc" })).map((n) => n.text), { kind: "query" }),
    root: method(z.object({}), (_, ctx) => ctx.root, { kind: "query" }),
  },
});
`;

/** A project checkout with the SDK linked in and these plugins written. */
async function checkout(plugins: Record<string, string>) {
  const root = await mkdtemp(join(tmpdir(), "repogo-checkout-"));
  const dir = join(root, ".repogo", "plugins");
  await mkdir(join(dir, "node_modules", "@repogo"), { recursive: true });
  await symlink(sdk, join(dir, "node_modules", "@repogo", "plugins"));
  for (const [name, source] of Object.entries(plugins)) {
    await mkdir(join(dir, name), { recursive: true });
    await writeFile(join(dir, name, "app.ts"), source);
  }
  return { root, dir };
}

type Client = {
  request: (method: string, params: unknown) => Promise<Reply>;
  pushes: DataPush[];
  /** Resolves with the next push that matches, or fails after a second. */
  nextPush: (match: (push: DataPush) => boolean) => Promise<DataPush>;
  close: () => void;
};

async function connect(socket: string): Promise<Client> {
  let nextID = 0;
  let inbox = "";
  const waiting = new Map<number, (reply: Reply) => void>();
  const pushes: DataPush[] = [];
  const watchers: (() => void)[] = [];
  const connection = await Bun.connect({
    unix: socket,
    socket: {
      data(_, chunk) {
        inbox += chunk.toString();
        let newline: number;
        while ((newline = inbox.indexOf("\n")) >= 0) {
          const message = JSON.parse(inbox.slice(0, newline));
          inbox = inbox.slice(newline + 1);
          if ("method" in message) {
            pushes.push(message);
            for (const watch of watchers) watch();
          } else {
            waiting.get(message.id)?.(message);
            waiting.delete(message.id);
          }
        }
      },
    },
  });
  return {
    pushes,
    request(method, params) {
      const id = ++nextID;
      return new Promise((resolve) => {
        waiting.set(id, resolve);
        connection.write(JSON.stringify({ id, method, params }) + "\n");
      });
    },
    nextPush(match) {
      const seen = pushes.length;
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error("no push within a second")), 1000);
        const watch = () => {
          const push = pushes.slice(seen).find(match);
          if (!push) return;
          clearTimeout(timer);
          resolve(push);
        };
        watchers.push(watch);
        watch();
      });
    },
    close: () => connection.end(),
  };
}

let stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops) stop();
  stops = [];
});

async function start() {
  const home = await mkdtemp(join(tmpdir(), "repogo-serve-"));
  const socket = join(home, "plugins.sock");
  const server = await serve({ socket, dataDir: join(home, "data") });
  const client = await connect(socket);
  stops.push(() => client.close(), () => server.stop());
  return { socket, client, dataDir: join(home, "data") };
}

const result = (reply: Reply) => {
  if ("error" in reply) throw new Error(`${reply.error.code}: ${reply.error.message}`);
  return reply.result as any;
};

test("the socket is the owner's alone", async () => {
  const { socket } = await start();
  expect((await stat(socket)).mode & 0o777).toBe(0o600);
});

test("list and call reach the checkout's plugins; data goes under the project", async () => {
  const { client, dataDir } = await start();
  const { root, dir } = await checkout({ notes: notesApp });
  const target = { project: "demo-1", dir };

  const manifest = result(await client.request("list", target));
  expect(manifest.plugins.map((plugin: { name: string }) => plugin.name)).toEqual(["notes"]);

  result(await client.request("call", { ...target, name: "notes.notes.create", input: { text: "a" } }));
  expect(result(await client.request("call", { ...target, name: "notes.texts" }))).toEqual(["a"]);
  expect(result(await client.request("call", { ...target, name: "notes.root" }))).toBe(root);
  expect((await stat(join(dataDir, "demo-1", "notes.db"))).isFile()).toBe(true);
});

test("a subscription gets a push when a write changes its result, and nothing otherwise", async () => {
  const { client } = await start();
  const { dir } = await checkout({ notes: notesApp });
  const target = { project: "demo-2", dir };

  const first = result(await client.request("subscribe", { ...target, subscription: "t1", name: "notes.texts" }));
  expect(first).toEqual({ subscription: first.subscription, revision: 1, result: [] });

  const pushed = client.nextPush((push) => push.params.subscription === first.subscription);
  result(await client.request("call", { ...target, name: "notes.notes.create", input: { text: "a" } }));
  expect((await pushed).params).toEqual({ subscription: first.subscription, revision: 2, result: ["a"] });

  // A write to another collection doesn't touch what the query read.
  result(await client.request("call", { ...target, name: "notes.tags.create", input: { name: "x" } }));
  await Bun.sleep(100);
  expect(client.pushes).toHaveLength(1);
});

test("a write in one checkout reaches the other checkout's subscriptions", async () => {
  const { client } = await start();
  const main = await checkout({ notes: notesApp });
  const worktree = await checkout({ notes: notesApp });

  const subscribed = result(await client.request("subscribe", { project: "demo-3", dir: main.dir, subscription: "t2", name: "notes.texts" }));
  const pushed = client.nextPush((push) => push.params.subscription === subscribed.subscription);
  result(await client.request("call", { project: "demo-3", dir: worktree.dir, name: "notes.notes.create", input: { text: "from the worktree" } }));
  expect((await pushed).params).toMatchObject({ revision: 2, result: ["from the worktree"] });
});

test("a reload runs the new code and pushes what changed", async () => {
  const { client } = await start();
  const { dir } = await checkout({ notes: notesApp });
  const target = { project: "demo-4", dir };
  const subscribed = result(await client.request("subscribe", { ...target, subscription: "t3", name: "notes.texts" }));

  await writeFile(join(dir, "notes", "app.ts"), notesApp.replace("(n) => n.text", "(n) => n.text.toUpperCase()").replace("ctx.root", '"new"'));
  result(await client.request("call", { ...target, name: "notes.notes.create", input: { text: "a" } }));
  await client.nextPush((push) => push.params.subscription === subscribed.subscription);

  const reloaded = client.nextPush((push) => push.params.subscription === subscribed.subscription && push.params.revision === 3);
  result(await client.request("reload", { project: "demo-4" }));
  expect((await reloaded).params).toMatchObject({ result: ["A"] });
  expect(result(await client.request("call", { ...target, name: "notes.root" }))).toBe("new");
});

test("after unsubscribe a write pushes nothing", async () => {
  const { client } = await start();
  const { dir } = await checkout({ notes: notesApp });
  const target = { project: "demo-5", dir };
  const subscribed = result(await client.request("subscribe", { ...target, subscription: "t4", name: "notes.texts" }));
  result(await client.request("unsubscribe", { subscription: subscribed.subscription }));
  result(await client.request("call", { ...target, name: "notes.notes.create", input: { text: "a" } }));
  await Bun.sleep(100);
  expect(client.pushes).toHaveLength(0);
});

test("a subscription only runs queries", async () => {
  const { client } = await start();
  const { dir } = await checkout({ notes: notesApp });
  const reply = await client.request("subscribe", { project: "demo-6", dir, subscription: "t5", name: "notes.notes.create", input: { text: "a" } });
  expect(reply).toMatchObject({ error: { code: "FORBIDDEN" } });
});

test("plugins that fail to load answer every request with why", async () => {
  const { client } = await start();
  const { dir } = await checkout({ broken: "export default {{{" });
  const reply = await client.request("list", { project: "demo-7", dir });
  expect(reply).toMatchObject({ error: { code: "INTERNAL" } });
  expect("error" in reply && reply.error.message).toContain("failed to load");
});

test("a malformed request is refused with its fields", async () => {
  const { client } = await start();
  const reply = await client.request("call", { project: "../escape", dir: "relative", name: "" });
  expect(reply).toMatchObject({ error: { code: "VALIDATION" } });
  expect("error" in reply && Object.keys(reply.error.fieldErrors ?? {})).toContain("/params/project");
});
