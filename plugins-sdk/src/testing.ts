import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Caller, CronEntry, CronStatus, Manifest, Notice } from "./context";
import type { PluginDefinition } from "./define";
import { MethodError } from "./errors";
import { captureConsole, releaseConsole, type LogRecord } from "./logs";
import { Runtime } from "./runtime";
import type { UploadedFile } from "./uploads";

// Plugins run for a test as the host runs them: real collections, kv, streams
// and buckets in a temporary folder, the same checks, confirms and crons.

export type TestCall = {
  /** False is a tap the user hasn't answered yet: a method with a `confirm` answers CONFIRM. */
  confirmed?: boolean;
  caller?: Caller;
  root?: string;
  /** The chat an agent calls from, kept on the call's log lines. */
  chat?: string;
  /** Files as an `<Upload>` sends them, given to the method as `input.files`. */
  files?: UploadedFile[];
};

export type TestRuntime = {
  /** Runs `<plugin>.<method>` and returns its answer, or throws its MethodError. */
  call: (method: string, input?: unknown, options?: TestCall) => Promise<unknown>;
  /** Sets the schedules, the whole set, as the host does from `~/.repogo/crons.json`. */
  setCrons: (crons: CronEntry[]) => CronStatus[];
  /** Runs every cron due at `now` and waits for them. */
  tickCrons: (now?: number) => Promise<void>;
  manifest: () => Manifest;
  /** Every `ctx.notify` so far, in order. */
  notices: Notice[];
  /** Every console line a plugin wrote so far, with the call it was written in; errors a method threw too. */
  logs: LogRecord[];
  /** Closes the data and deletes its folder. */
  close: () => Promise<void>;
};

/** What `ctx.mcp` asks the host: the tool, its input, and whether a query or a confirmed method asks. */
export type TestMCPCall = { project: string; server: string; tool: string; input: unknown; read: boolean; confirmed: boolean };

/** Stands in for the user's MCP servers: answers a tool's result, or throws to refuse as a tool error. */
export type TestMCP = (call: TestMCPCall) => unknown;

/** Stands in for the host's methods (`chats.start`, `git.status`, …) behind `ctx.host` and `ctx.agent`; throw a MethodError to refuse. */
export type TestHost = (method: string, params: unknown) => unknown;

/**
 * `const app = await testRuntime({ notes })`, then `await app.call("notes.add", { text: "hi" })`.
 * `ctx.root` is an empty temporary folder unless `root` names a fixture project;
 * `mcp` answers `ctx.mcp`, `host` the other host methods. Console lines still print.
 */
export async function testRuntime(plugins: Record<string, PluginDefinition>, options: { root?: string; mcp?: TestMCP; host?: TestHost } = {}): Promise<TestRuntime> {
  const dataDir = await mkdtemp(join(tmpdir(), "repogo-plugin-test-"));
  const project = join(dataDir, "project");
  await mkdir(project);
  const host = options.mcp || options.host ? testHost(options.mcp, options.host) : undefined;
  const runtime = await Runtime.open({ dataDir: join(dataDir, "data"), root: options.root ?? project, host });
  const notices: Notice[] = [];
  runtime.onNotify((notice) => notices.push(notice));
  const logs: LogRecord[] = [];
  captureConsole((entry) => logs.push(entry), { passThrough: true });
  try {
    for (const [name, plugin] of Object.entries(plugins)) await runtime.add(name, plugin);
  } catch (error) {
    releaseConsole();
    runtime.close();
    await rm(dataDir, { recursive: true, force: true });
    throw error;
  }
  return {
    call: async (method, input = {}, call = {}) => (await runtime.call(method, input, call)).result,
    setCrons: (crons) => runtime.setCrons(crons),
    tickCrons: async (now) => { await Promise.all(runtime.tickCrons(now)); },
    manifest: () => runtime.manifest(),
    notices,
    logs,
    close: async () => {
      releaseConsole();
      runtime.close();
      await rm(dataDir, { recursive: true, force: true });
    },
  };
}

/** The host as a test sees it: `mcp.call` goes to `mcp`, other methods to `host`; what neither serves is refused. */
function testHost(mcp?: TestMCP, host?: TestHost) {
  return async (_plugin: string, method: string, params: unknown) => {
    if (method !== "mcp.call") {
      if (!host) throw new MethodError("FORBIDDEN", `This test runtime doesn't serve ${method}.`);
      return host(method, params);
    }
    if (!mcp) throw new MethodError("FORBIDDEN", "This test runtime has no MCP servers.");
    try {
      return { result: await mcp(params as TestMCPCall) };
    } catch (error) {
      return { error: { code: "TOOL_ERROR", message: error instanceof Error ? error.message : String(error) } };
    }
  };
}
