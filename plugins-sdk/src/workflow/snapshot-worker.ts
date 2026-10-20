import { isDeepStrictEqual } from "node:util";
import type { PluginDefinition } from "../define";
import { captureConsole } from "../logs";
import { MethodError, toCallError } from "../errors";
import { Runtime } from "../runtime";
import { tracked } from "../tracking";
import { RunJournal } from "./journal";
import { runWorkflow } from "./runner";
import type { SnapshotMessage, SnapshotStart } from "./contracts";
import { describe, summarize } from "./summary";
export * from "../index";

export function runSnapshot(plugins: Record<string, PluginDefinition>) {
  if (!process.send) throw new Error("Workflow executors require an IPC parent.");
  const post = (message: SnapshotMessage) => process.send!(message);
  const waiting = new Map<number, { resolve: (value: unknown) => void; reject: (error: Error) => void }>();
  let next = 0;
  let launched = false;
  const cancel = new AbortController();
  const suspend = new AbortController();
  let finished = false;
  process.on("disconnect", () => {
    if (finished) return;
    suspend.abort();
    setTimeout(() => process.exit(1), 1000).unref();
  });
  captureConsole(entry => post({ type: "log", records: [entry] }));
  process.on("message", async (message: SnapshotStart | { type: "cancel" } | { type: "suspend" } | { type: "hosted"; id: number; result?: unknown; error?: { code: string; message: string } }) => {
    if (message.type === "suspend") { suspend.abort(); return; }
    if (message.type === "cancel") { cancel.abort(); return; }
    if (message.type === "hosted") {
      const call = waiting.get(message.id); waiting.delete(message.id);
      if (message.error) call?.reject(new MethodError(message.error.code as any, message.error.message));
      else call?.resolve(message.result);
      return;
    }
    if (launched) return;
    launched = true;
    let failed = false;
    let runtime: Runtime | null = null;
    let journal: RunJournal | null = null;
    try {
      const run = message.run;
      runtime = await Runtime.open({ dataDir: message.dataDir, root: run.root, host: (plugin, method, params) => {
        const id = ++next;
        return new Promise((resolve, reject) => { waiting.set(id, { resolve, reject }); post({ type: "host", id, plugin, method, params }); });
      } });
      runtime.onInvalidate(tags => post({ type: "invalidated", tags }));
      runtime.onNotify(notice => post({ type: "notice", notice }));
      for (const [name, definition] of Object.entries(plugins)) await runtime.add(name, definition);
      journal = await RunJournal.openExisting(message.directory);
      if (journal.state.status === "pending" || !isDeepStrictEqual(journal.state.metadata, run)) throw new Error("The workflow journal does not match its execution identity.");
      const opened = journal;
      const publish = () => { const state = opened.state; post({ type: "state", summary: summarize(run, state), detail: describe(state) }); };
      const remove = journal.onChange(publish);
      try {
        const execution = await tracked(false, tags => post({ type: "invalidated", tags }), () => runWorkflow(opened,
          { workflow: run.workflow, buildId: run.buildId, input: run.input as any },
          async (input, context) => await runtime!.executeWorkflow(run.plugin, run.workflow, input, { ...context, root: run.root, runId: run.id, caller: run.caller }) as any,
          { signal: cancel.signal, suspend: suspend.signal, timeout: run.timeoutMs, maxOffline: run.maxOfflineMs },
        ));
        if (execution.access.writes.size) post({ type: "invalidated", tags: [...execution.access.writes] });
        if (!execution.ok && !["PAUSED", "WAITING", "FAILED", "UNKNOWN", "CANCELLED", "EXPIRED"].includes((execution.error as { code?: string })?.code ?? "")) throw execution.error;
      } finally { remove(); }
      publish();
    } catch (error) { failed = true; post({ type: "error", message: toCallError(error).message }); }
    finally {
      try { await journal?.close(); } catch (error) { failed = true; post({ type: "error", message: String(error) }); }
      runtime?.close();
      finished = true;
      const code = failed ? 1 : 0;
      const timer = setTimeout(() => process.exit(code || 1), 1000);
      process.send!({ type: "done" }, () => {
        clearTimeout(timer); process.disconnect?.(); process.exit(code);
      });
    }
  });
}
