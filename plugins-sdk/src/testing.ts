import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Caller, Manifest, Notice } from "./context";
import type { PluginDefinition } from "./define";
import { Runtime } from "./runtime";

// Plugins run for a test as the host runs them: real collections, kv, streams
// and buckets in a temporary folder, the same checks, confirms and crons.

export type TestCall = {
  /** False is a tap the user hasn't answered yet: a method with a `confirm` answers CONFIRM. */
  confirmed?: boolean;
  caller?: Caller;
  root?: string;
};

export type TestRuntime = {
  /** Runs `<plugin>.<method>` and returns its answer, or throws its MethodError. */
  call: (method: string, input?: unknown, options?: TestCall) => Promise<unknown>;
  /** Runs every cron due at `now` and waits for them. */
  tickCrons: (now?: number) => Promise<void>;
  manifest: () => Manifest;
  /** Every `ctx.notify` so far, in order. */
  notices: Notice[];
  /** Closes the data and deletes its folder. */
  close: () => Promise<void>;
};

/**
 * `const app = await testRuntime({ notes })`, then `await app.call("notes.add", { text: "hi" })`.
 * `ctx.root` is an empty temporary folder unless `root` names a fixture project.
 */
export async function testRuntime(plugins: Record<string, PluginDefinition>, options: { root?: string } = {}): Promise<TestRuntime> {
  const dataDir = await mkdtemp(join(tmpdir(), "repogo-plugin-test-"));
  const project = join(dataDir, "project");
  await mkdir(project);
  const runtime = await Runtime.open({ dataDir: join(dataDir, "data"), root: options.root ?? project });
  const notices: Notice[] = [];
  runtime.onNotify((notice) => notices.push(notice));
  try {
    for (const [name, plugin] of Object.entries(plugins)) await runtime.add(name, plugin);
  } catch (error) {
    runtime.close();
    await rm(dataDir, { recursive: true, force: true });
    throw error;
  }
  return {
    call: async (method, input = {}, call = {}) => (await runtime.call(method, input, call)).result,
    tickCrons: async (now) => { await Promise.all(runtime.tickCrons(now)); },
    manifest: () => runtime.manifest(),
    notices,
    close: async () => {
      runtime.close();
      await rm(dataDir, { recursive: true, force: true });
    },
  };
}
