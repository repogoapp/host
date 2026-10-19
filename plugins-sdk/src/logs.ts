import { AsyncLocalStorage } from "node:async_hooks";
import { format } from "node:util";
import type { Caller } from "./context";

// A plugin writes `console.log`; the runtime turns each call into a record
// with the run it happened in, and the Worker sends the records to the host,
// which keeps them for agents and cards to read.

export type LogLevel = "debug" | "info" | "warn" | "error";

export type LogRecord = {
  /** ms since the epoch. */
  at: number;
  plugin: string;
  level: LogLevel;
  /** `<plugin>.<method>` of the run the line was written in; empty outside a run. */
  method: string;
  caller: Caller | "";
  /** One id per outermost call, so a call's lines read together. */
  callId: string;
  /** The chat whose agent made the call, for `caller: "agent"`. */
  chatId: string;
  /** The arguments formatted as the console prints them, at most `textLimit` characters. */
  text: string;
  /** The arguments as given, as JSON can carry them. */
  data: unknown[];
};

/** What a line is attributed to: the plugin, and the run when there is one. */
export type LogContext = {
  plugin: string;
  method?: string;
  caller?: Caller;
  callId?: string;
  chatId?: string;
};

/** The run the code writing a line is in; the runtime sets it around every method, handler and cron. */
export const logContext = new AsyncLocalStorage<LogContext>();

/** The outermost runs going now, oldest first: what a line with no run context is laid to. */
const activeRuns: LogContext[] = [];

/**
 * Runs `work` as an outermost run under `context`. An error nothing catches
 * reaches its handler with no async context, so the newest run going is the
 * best guess at whose it is.
 */
export async function runWith<T>(context: LogContext, work: () => Promise<T>): Promise<T> {
  activeRuns.push(context);
  try {
    return await logContext.run(context, work);
  } finally {
    const i = activeRuns.lastIndexOf(context);
    if (i >= 0) activeRuns.splice(i, 1);
  }
}

/** A line longer than this is cut: a dumped payload is read in the plugin's own data, not its log. */
export const textLimit = 8 * 1024;

type Sink = (record: LogRecord) => void;

const levels: Record<"log" | "info" | "debug" | "warn" | "error", LogLevel> = {
  log: "info", info: "info", debug: "debug", warn: "warn", error: "error",
};

const original = { log: console.log, info: console.info, debug: console.debug, warn: console.warn, error: console.error };

let sink: Sink | null = null;

/**
 * Replaces `console.log`, `info`, `debug`, `warn` and `error` with ones that
 * record each call. `passThrough` keeps printing them too, for tests.
 */
export function captureConsole(to: Sink, options: { passThrough?: boolean } = {}) {
  sink = to;
  for (const name of Object.keys(levels) as (keyof typeof levels)[]) {
    console[name] = (...args: unknown[]) => {
      if (options.passThrough) original[name](...args);
      record(levels[name], args);
    };
  }
}

/** Puts the console back and stops recording. */
export function releaseConsole() {
  sink = null;
  Object.assign(console, original);
}

/**
 * Records one line under `context`, else the current run, else the newest run
 * going, as an error nothing caught lands in. With no capture installed the
 * line prints as the console would.
 */
export function record(level: LogLevel, args: unknown[], context?: LogContext) {
  const where = context ?? logContext.getStore() ?? activeRuns.at(-1);
  // Without a capture, or outside every run, the line is the server's own, printed as it was.
  if (!sink || !where) {
    original[level === "info" ? "log" : level](...args);
    return;
  }
  const text = format(...args);
  sink({
    at: Date.now(),
    plugin: where.plugin,
    level,
    method: where.method ?? "",
    caller: where.caller ?? "",
    callId: where.callId ?? "",
    chatId: where.chatId ?? "",
    text: text.length > textLimit ? text.slice(0, textLimit) : text,
    data: args.map(carryable),
  });
}

/** An argument as JSON carries it: an Error keeps its name, message and stack. */
function carryable(value: unknown): unknown {
  if (value instanceof Error) {
    return { name: value.name, message: value.message, stack: value.stack ?? "" };
  }
  if (typeof value === "bigint") return value.toString();
  if (typeof value === "function" || typeof value === "symbol" || value === undefined) return String(value);
  try {
    JSON.stringify(value);
    return value;
  } catch {
    return format(value);
  }
}
