#!/usr/bin/env bun
// `serve` is the plugins server the host keeps running (serve.ts). `list` and
// `call` answer once and exit, for running plugins by hand. Plugin logs go to stderr.
//
//   run.ts serve                           → listens on REPOGO_PLUGINS_SOCKET
//   run.ts list                            → the manifest
//   run.ts call <method> [input] [--query] → { result, reads, invalidates } or { error }

import "./self";
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { join, resolve } from "node:path";
import { toCallError } from "./errors";
import { Runtime } from "./runtime";
import { serve } from "./serve";

export type Dirs = { pluginsDir: string; dataDir: string };

export function dirsFromEnv(env: Record<string, string | undefined> = process.env): Dirs {
  const home = join(homedir(), ".repogo");
  return {
    pluginsDir: resolve(env.REPOGO_PLUGINS_DIR ?? join(home, "plugins")),
    dataDir: resolve(env.REPOGO_DATA_DIR ?? join(home, "data")),
  };
}

export async function run(args: string[], dirs: Dirs): Promise<{ output: unknown; ok: boolean }> {
  const runtime = await Runtime.open({ dataDir: dirs.dataDir });
  try {
    if (existsSync(dirs.pluginsDir)) await runtime.loadDir(dirs.pluginsDir);
    const [command, name, input] = args.filter((arg) => !arg.startsWith("--"));
    if (command === "list") return { output: runtime.manifest(), ok: true };
    if (command === "call" && name) {
      const expect = args.includes("--query") ? "query" : undefined;
      const result = await runtime.call(name, input ? JSON.parse(input) : {}, { expect });
      return { output: result, ok: true };
    }
    return { output: { error: { code: "VALIDATION", message: "usage: run.ts list | call <method> [input] [--query]" } }, ok: false };
  } catch (error) {
    return { output: { error: toCallError(error) }, ok: false };
  } finally {
    runtime.close();
  }
}

if (import.meta.main && process.argv[2] === "serve") {
  const home = join(homedir(), ".repogo");
  const server = await serve({
    socket: resolve(process.env.REPOGO_PLUGINS_SOCKET ?? join(home, "plugins.sock")),
    dataDir: resolve(process.env.REPOGO_DATA_DIR ?? join(home, "plugin-data")),
  });
  const stop = () => {
    server.stop();
    process.exit(0);
  };
  process.on("SIGTERM", stop);
  process.on("SIGINT", stop);
  // The host holds our stdin open; it closing means the host is gone, however it went.
  if (process.env.REPOGO_PLUGINS_PARENT === "stdin") {
    process.stdin.on("end", stop);
    process.stdin.on("close", stop);
    process.stdin.resume();
  }
} else if (import.meta.main) {
  const { output, ok } = await run(process.argv.slice(2), dirsFromEnv());
  process.stdout.write(JSON.stringify(output) + "\n");
  process.exit(ok ? 0 : 1);
}
