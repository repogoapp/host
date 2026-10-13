#!/usr/bin/env bun
// One answer per process: the host runs this for each call and reads the JSON
// on stdout, so nothing stays running between calls. App logs go to stderr.
//
//   run.ts list                         → the manifest
//   run.ts call <method> [input] [--query] → { result, reads, invalidates } or { error }

import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { join, resolve } from "node:path";
import { toCallError } from "./errors";
import { Runtime } from "./runtime";

export type Dirs = { appsDir: string; dataDir: string };

export function dirsFromEnv(env: Record<string, string | undefined> = process.env): Dirs {
  const home = join(homedir(), ".repogo");
  return {
    appsDir: resolve(env.REPOGO_APPS_DIR ?? join(home, "apps")),
    dataDir: resolve(env.REPOGO_DATA_DIR ?? join(home, "data")),
  };
}

export async function run(args: string[], dirs: Dirs): Promise<{ output: unknown; ok: boolean }> {
  const runtime = await Runtime.open({ dataDir: dirs.dataDir });
  try {
    if (existsSync(dirs.appsDir)) await runtime.loadDir(dirs.appsDir);
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

if (import.meta.main) {
  const { output, ok } = await run(process.argv.slice(2), dirsFromEnv());
  process.stdout.write(JSON.stringify(output) + "\n");
  process.exit(ok ? 0 : 1);
}
