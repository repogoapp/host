import { isDeepStrictEqual } from "node:util";
import { mkdir, readdir, rm, stat } from "node:fs/promises";
import { dirname, join } from "node:path";
import { z } from "zod";
import { MethodError, toCallError } from "../errors";
import { captureArtifact, publishArtifact, verifyArtifact } from "./artifacts";
import { WorkflowCatalog, type CatalogRun } from "./catalog";
import { networkAvailable, nextAvailability, type NetworkSource } from "./availability";
import { systemClock, type WorkflowClock } from "./clock";
import { RunMetadataSchema, RunSummarySchema, type RunMetadata, type RunSummary, type SnapshotMessage, type WorkflowCallbacks, type WorkflowRequest } from "./contracts";
import { settleInterrupted } from "./interrupted";
import { RunJournal } from "./journal";
import { workflowTag } from "./register";
import { activeStatuses, terminalStatuses, describe, summarize, briefError, unavailableDetail } from "./summary";

const scopeMatches = (run: RunMetadata, request: WorkflowRequest) =>
  run.dir === request.dir && run.plugin === request.plugin && run.namespace === request.namespace && run.workflow === request.workflow && run.root === request.root;
const runReserve = 130 * 1024 * 1024;

type Active = { worker: ReturnType<typeof Bun.spawn>; ended: Promise<void>; finish: () => void; error: string | null; acknowledge?: (summary: RunSummary) => void };
export class WorkflowService {
  private queue: Promise<unknown> = Promise.resolve();
  private details = new Map<string, ReturnType<typeof describe>>();
  private rows = new Map<string, CatalogRun>();
  private unrecovered = new Set<string>();
  private cancelling = new Set<string>();
  private active = new Map<string, Active>();
  private observers = new Set<(summary: RunSummary) => void>();
  private timer: (() => void) | null = null;
  private closing = false;
  private started = false;
  private constructor(
    private dataDir: string, private catalog: WorkflowCatalog, private callbacks: WorkflowCallbacks,
    private clock: WorkflowClock, private storageLimit: number, private network: NetworkSource,
  ) {}
  static async open(dataDir: string, callbacks: WorkflowCallbacks, options: { clock?: WorkflowClock; storageLimit?: number; network?: NetworkSource } = {}) {
    const service = new WorkflowService(dataDir, await WorkflowCatalog.open(join(dataDir, ".workflows")), callbacks, options.clock ?? systemClock, options.storageLimit ?? 512 * 1024 * 1024, options.network ?? networkAvailable);
    try {
      for (const row of await service.catalog.all()) {
        row.run = RunMetadataSchema.parse(row.run);
        service.rows.set(row.run.id, row);
      }
      await service.recoverCatalog();
      return service;
    } catch (error) { await service.catalog.close(); throw error; }
  }
  private get builds() { return join(this.dataDir, ".workflows", "builds"); }
  private directory(run: RunMetadata) { return join(this.dataDir, run.namespace, "workflows", run.id); }
  private serial<T>(work: () => Promise<T>): Promise<T> {
    const result = this.queue.then(work);
    this.queue = result.catch(() => {});
    return result;
  }
  observe(listener: (summary: RunSummary) => void) { this.observers.add(listener); return () => { this.observers.delete(listener); }; }
  start() { if (!this.closing) { this.started = true; this.wake(); } }
  private wake() {
    this.timer?.();
    this.timer = null;
    if (!this.started || this.closing) return;
    let due = this.clock.now() + (this.rows.size ? 5000 : 60_000);
    for (const row of this.rows.values()) {
      if (!activeStatuses.has(row.summary.status) && row.summary.status !== "blocked") continue;
      due = Math.min(due, this.clock.now() + row.run.maxOfflineMs / 2);
      if (this.active.has(row.run.id) || this.active.size >= 4) continue;
      due = Math.min(due, row.summary.nextAttemptAt ? Date.parse(row.summary.nextAttemptAt) : this.clock.now());
      if (row.summary.status !== "blocked" && row.summary.deadlineAt) due = Math.min(due, Date.parse(row.summary.deadlineAt));
    }
    this.timer = this.clock.schedule(due, () => { void this.tick().finally(() => this.wake()).catch(() => {}); });
  }
  private async save(row: CatalogRun) {
    this.rows.set(row.run.id, row);
    await this.catalog.put(row);
    this.callbacks.invalidate([workflowTag(row.run.namespace)]);
    for (const observer of this.observers) { try { observer(row.summary); } catch (error) { console.error(error); } }
  }
  async request(request: WorkflowRequest): Promise<unknown> {
    if (this.closing) throw new MethodError("INTERNAL", "The workflow service is closing.");
    if (request.operation === "list") {
      const rows = [...this.rows.values()].filter(row => scopeMatches(row.run, request) && (!request.input.status || row.summary.status === request.input.status))
        .sort((a, b) => b.run.createdAt.localeCompare(a.run.createdAt) || b.run.id.localeCompare(a.run.id));
      const cursor = request.input.cursor as string | undefined;
      const start = cursor ? rows.findIndex(row => row.run.id === cursor) + 1 : 0;
      if (cursor && start === 0) throw new MethodError("VALIDATION", "Invalid workflow cursor.");
      const count = Math.min(100, Math.max(1, Number(request.input.limit ?? 20)));
      const page = rows.slice(start, start + count);
      return { items: page.map(row => row.summary), cursor: start + count < rows.length ? page.at(-1)!.run.id : null };
    }
    if (request.operation === "start") return this.serial(() => this.create(request));
    const row = this.rows.get(String(request.input.id));
    if (!row || !scopeMatches(row.run, request)) throw new MethodError("NOT_FOUND", "No workflow run in this project.");
    if (request.operation === "retry") return this.serial(async () => {
      if (await this.bytes(row.run.id) > this.storageLimit) throw new MethodError("VALIDATION", "Workflow storage is full.");
      const active = this.active.get(row.run.id);
      if (active) { await active.ended; this.active.delete(row.run.id); }
      const journal = await this.openJournal(row.run);
      try {
        const stepId = journal.state.currentStep;
        if (!stepId) throw new MethodError("VALIDATION", "This run has no failed query to retry.");
        try { await journal.append({ type: "run.retryRequested", stepId }); }
        catch (error) { throw new MethodError("VALIDATION", toCallError(error).message); }
        row.summary = summarize(row.run, journal.state); row.terminal = null; row.error = null; row.seen = this.clock.now();
        await this.save(row); this.wake(); return row.summary;
      } finally { await journal.close(); }
    });
    if (request.operation === "cancel") {
      const active = this.active.get(row.run.id);
      if (active) return this.cancelActive(row, active);
      return this.serial(() => this.cancelIdle(row));
    }
    return this.serial(async () => {
      // The active owner publishes state; get never takes its lock or trusts its snapshot.
      const fallback = unavailableDetail(row.run.input, row.error);
      if (this.active.has(row.run.id)) return { ...row.summary, ...(this.details.get(row.run.id) ?? fallback) };
      let journal: RunJournal;
      try { journal = await this.openJournal(row.run); }
      catch (error) {
        if (row.summary.status === "blocked" || row.error) return { ...row.summary, ...fallback };
        throw error;
      }
      try {
        const state = journal.state;
        const summary = row.summary.status === "blocked" ? row.summary : summarize(row.run, state);
        return { ...summary, ...describe(state), error: briefError(state.error ?? (row.error ? { message: row.error } : null)) };
      } finally { await journal.close(); }
    });
  }
  private async cancelIdle(row: CatalogRun) {
    const journal = await this.openJournal(row.run);
    try {
      if (activeStatuses.has(journal.state.status)) {
        await journal.append({ type: "run.cancelRequested" });
        await settleInterrupted(journal, "CANCELLED");
        await journal.append({ type: "run.cancelled" });
      }
      row.summary = summarize(row.run, journal.state);
      if (terminalStatuses.has(row.summary.status)) row.terminal ??= Date.parse(row.summary.updatedAt);
      await this.save(row); return row.summary;
    } finally { await journal.close(); }
  }
  private async cancelActive(row: CatalogRun, active: Active) {
    this.cancelling.add(row.run.id);
    const ack = Promise.withResolvers<RunSummary>();
    active.acknowledge = ack.resolve;
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      active.worker.send({ type: "cancel" });
      const accepted = await Promise.race([ack.promise, active.worker.exited.then(() => null), new Promise<null>(resolve => {
        timer = setTimeout(() => { active.worker.kill("SIGKILL"); resolve(null); }, 1000);
      })]);
      if (accepted) return accepted;
      await active.worker.exited;
      return await this.serial(async () => { this.active.delete(row.run.id); return this.cancelIdle(row); });
    } finally {
      if (timer) clearTimeout(timer);
      delete active.acknowledge;
      this.cancelling.delete(row.run.id);
      this.wake();
    }
  }
  private async create(request: WorkflowRequest) {
    const input = z.json().parse(request.input.input);
    const key = request.input.key ? String(request.input.key) : null;
    const found = key ? [...this.rows.values()].find(row => scopeMatches(row.run, request) && row.run.key === key) : undefined;
    if (found) {
      if (!isDeepStrictEqual(found.run.input, input)) throw new MethodError("VALIDATION", "This workflow key already has different input.");
      return found.summary;
    }
    if (!request.settings) throw new MethodError("VALIDATION", "Workflow settings are required.");
    const artifact = await captureArtifact(request.dir);
    const existing = await stat(join(this.builds, artifact.id, "worker.js")).catch(() => null);
    if (await this.bytes() + (existing ? 0 : artifact.bytes.length) + runReserve > this.storageLimit) throw new MethodError("VALIDATION", "Workflow storage is full; retained runs and required builds cannot be evicted.");
    await publishArtifact(this.builds, artifact);
    let run = RunMetadataSchema.parse({ dir: request.dir, root: request.root, namespace: request.namespace, plugin: request.plugin, workflow: request.workflow, caller: request.caller ?? "code", input, key, id: crypto.randomUUID(), buildId: artifact.id, createdAt: new Date(this.clock.now()).toISOString(), ...request.settings });
    const journal = await RunJournal.open(this.directory(run), this.clock);
    let summary: RunSummary;
    try {
      const { workflow, buildId, input: savedInput, timeoutMs, createdAt, ...location } = run;
      await journal.append({ type: "run.started", workflow, buildId, input: savedInput, timeoutMs, location });
      run = journal.state.metadata!;
      summary = summarize(run, journal.state);
    } finally { await journal.close(); }
    const row = { run, summary, seen: this.clock.now(), terminal: null, error: null };
    await this.save(row);
    if (!await this.catalog.availability()) await this.catalog.heartbeat(nextAvailability(null, this.clock.now(), this.network()));
    this.wake();
    return summary;
  }
  tick(): Promise<void> { return this.serial(async () => {
    if (this.closing) return;
    const now = this.clock.now();
    if (!this.rows.size) { await this.collect(); return; }
    const availability = await this.catalog.availability();
    const next = nextAvailability(availability, now, this.network());
    const offlineSince = availability?.offlineSince ?? next.offlineSince;
    for (const row of [...this.rows.values()]) {
      const networkExpired = offlineSince !== null && now - Math.max(offlineSince, Date.parse(row.run.createdAt)) >= row.run.maxOfflineMs;
      const unavailable = availability !== null && now - Math.max(availability.seen, Date.parse(row.run.createdAt)) >= row.run.maxOfflineMs;
      const deadlineReached = row.summary.deadlineAt !== null && now >= Date.parse(row.summary.deadlineAt);
      if (this.active.has(row.run.id)) {
        if (!networkExpired && !unavailable && !deadlineReached) { row.seen = now; continue; }
        const active = this.active.get(row.run.id)!;
        active.worker.kill("SIGKILL"); await active.worker.exited;
        this.active.delete(row.run.id);
      }
      if (row.terminal !== null && now - row.terminal < row.run.retentionMs) continue;
      if (availability && !networkExpired && !unavailable && !deadlineReached && row.summary.nextAttemptAt && Date.parse(row.summary.nextAttemptAt) > now) {
        row.seen = now; continue;
      }
      const blockedUntil = row.summary.status === "blocked" ? row.summary.nextAttemptAt : null;
      try {
        const journal = await this.openJournal(row.run);
        try {
          const state = journal.state;
          const offline = networkExpired || unavailable || (!availability && now - row.seen >= row.run.maxOfflineMs);
          const expired = state.deadlineAt !== null && now >= Date.parse(state.deadlineAt);
          if (activeStatuses.has(state.status) && (offline || expired)) {
            await settleInterrupted(journal, "EXPIRED");
            await journal.append({ type: "run.expired", reason: offline ? "Environment unavailable beyond the workflow offline limit." : "Workflow deadline reached." });
          }
          row.summary = summarize(row.run, journal.state);
          if (activeStatuses.has(row.summary.status) && blockedUntil && Date.parse(blockedUntil) > now) row.summary = { ...row.summary, status: "blocked", nextAttemptAt: blockedUntil };
          row.seen = now;
          if (journal.state.endedAt) row.terminal ??= Date.parse(journal.state.endedAt);
          await this.save(row);
        } finally { await journal.close(); }
      } catch (error) {
        row.error = toCallError(error).message;
        if (!terminalStatuses.has(row.summary.status)) {
          const expired = now - row.seen >= row.run.maxOfflineMs || (row.summary.deadlineAt !== null && now >= Date.parse(row.summary.deadlineAt));
          row.summary = { ...row.summary, status: expired ? "expired" : "blocked", nextAttemptAt: expired ? null : new Date(now + 60_000).toISOString() };
          if (expired) row.terminal ??= now;
        }
        await this.save(row);
      }
      if (row.terminal !== null && now - row.terminal >= row.run.retentionMs) {
        await this.catalog.remove(row.run.id); this.rows.delete(row.run.id); this.details.delete(row.run.id);
        await rm(this.directory(row.run), { recursive: true, force: true });
        this.callbacks.invalidate([workflowTag(row.run.namespace)]);
        continue;
      }
      if (!this.cancelling.has(row.run.id) && activeStatuses.has(row.summary.status) && this.active.size < 4 && (!row.summary.nextAttemptAt || now >= Date.parse(row.summary.nextAttemptAt))) await this.launch(row);
    }
    await this.catalog.heartbeat(next);
    await this.collect();
  }); }
  private async launch(row: CatalogRun) {
    try {
      const path = await verifyArtifact(this.builds, row.run.buildId);
      const worker = Bun.spawn([process.execPath, "--no-env-file", path], {
        cwd: dirname(path), stdin: "ignore", stdout: "ignore", stderr: "pipe",
        ipc: message => onMessage(message as SnapshotMessage), serialization: "advanced",
      });
      const stderr = stderrTail(worker.stderr);
      let finish!: () => void;
      const ended = new Promise<void>(resolve => { finish = resolve; });
      const active: Active = { worker, ended, finish, error: null };
      this.active.set(row.run.id, active);
      let finished = false;
      const complete = () => {
        if (finished) return; finished = true;
        active.finish();
        if (this.closing) return;
        void this.serial(async () => {
          this.active.delete(row.run.id);
          row.error = active.error;
          if (active.error && !terminalStatuses.has(row.summary.status)) { row.summary = { ...row.summary, status: "blocked", nextAttemptAt: new Date(this.clock.now() + 60_000).toISOString() }; await this.save(row); }
        }).then(() => this.tick()).then(() => this.wake()).catch(() => {});
      };
      const onMessage = (message: SnapshotMessage) => {
        if (this.closing) return;
        if (message.type === "host") {
          void this.callbacks.host(message.plugin, message.method, message.params).then(
            result => worker.send({ type: "hosted", id: message.id, result }),
            error => worker.send({ type: "hosted", id: message.id, error: toCallError(error) }),
          ).catch(() => {});
        } else if (message.type === "state") {
          const summary = RunSummarySchema.safeParse(message.summary);
          if (!summary.success || summary.data.id !== row.run.id || summary.data.buildId !== row.run.buildId || summary.data.workflow !== row.run.workflow || summary.data.createdAt !== row.run.createdAt) {
            active.error = "The workflow executor reported an invalid run identity.";
            return;
          }
          if (message.detail.cancelRequested) active.acknowledge?.(message.summary);
          void this.serial(async () => { if (this.active.get(row.run.id) !== active) return; row.summary = message.summary; this.details.set(row.run.id, message.detail); row.seen = this.clock.now(); await this.save(row); });
        } else if (message.type === "invalidated") this.callbacks.invalidate(message.tags);
        else if (message.type === "notice") this.callbacks.notify(message.notice);
        else if (message.type === "log") this.callbacks.logs(row.run.dir, message.records);
        else if (message.type === "error") active.error = message.message;
        else if (message.type === "done") { /* Process exit releases the run lock. */ }
      };
      void worker.exited.then(async code => { if (code !== 0 && !active.error) active.error = (await stderr).slice(-4000) || `Workflow executor exited ${code}.`; complete(); });
      worker.send({ type: "start", run: row.run, directory: this.directory(row.run), dataDir: this.dataDir });
    } catch (error) {
      row.summary = { ...row.summary, status: "blocked", nextAttemptAt: new Date(this.clock.now() + 60_000).toISOString() };
      row.error = toCallError(error).message;
      await this.save(row);
    }
  }
  private async openJournal(run: RunMetadata) {
    const path = this.directory(run);
    if (!(await stat(join(path, "events.jsonl"))).size) throw new Error("The workflow journal is missing or empty; execution is blocked.");
    return RunJournal.openExisting(path, this.clock);
  }
  private async recoverCatalog() {
    let complete = true;
    const namespaces = await readdir(this.dataDir, { withFileTypes: true });
    for (const namespace of namespaces) {
      if (!namespace.isDirectory() || !/^[a-z0-9][a-z0-9.-]*$/.test(namespace.name)) continue;
      const base = join(this.dataDir, namespace.name, "workflows");
      const entries = await readdir(base, { withFileTypes: true }).catch(error => {
        if ((error as NodeJS.ErrnoException).code !== "ENOENT") {
          complete = false;
          if (!this.unrecovered.has(base)) console.error("Workflow discovery blocked:", base, toCallError(error).message);
          this.unrecovered.add(base);
        }
        return [];
      });
      for (const entry of entries) {
        if (!entry.isDirectory() || !z.string().uuid().safeParse(entry.name).success || this.rows.has(entry.name)) continue;
        const directory = join(base, entry.name);
        try {
          if (!(await stat(join(directory, "events.jsonl")).catch(() => null))?.size) continue;
          const journal = await RunJournal.openExisting(directory, this.clock);
          try {
            const state = journal.state;
            const run = state.metadata;
            if (!run || run.id !== entry.name || run.namespace !== namespace.name) throw new Error("Workflow journal location is invalid.");
            const row: CatalogRun = { run, summary: summarize(run, state), seen: Date.parse(state.updatedAt!), terminal: state.endedAt ? Date.parse(state.endedAt) : null, error: null };
            await this.catalog.put(row); this.rows.set(run.id, row);
            this.unrecovered.delete(directory);
          } finally { await journal.close(); }
        } catch (error) {
          complete = false;
          if (!this.unrecovered.has(directory)) console.error("Workflow discovery blocked:", directory, toCallError(error).message);
          this.unrecovered.add(directory);
        }
      }
    }
    return complete;
  }
  private async collect() {
    if (!await this.recoverCatalog()) return;
    const referenced = new Set([...this.rows.values()].map(row => row.run.buildId));
    const entries = await readdir(this.builds).catch(() => [] as string[]);
    for (const id of entries) if (/^[a-f0-9]{64}$/.test(id) && !referenced.has(id)) await rm(join(this.builds, id), { recursive: true, force: true });
  }
  private async bytes(reserveId?: string): Promise<number> {
    const tree = async (path: string): Promise<number> => {
      const entries = await readdir(path, { withFileTypes: true }).catch(() => []);
      let size = 0;
      for (const entry of entries) { const name = join(path, entry.name); size += entry.isDirectory() ? await tree(name) : (await stat(name)).size; }
      return size;
    };
    let bytes = await tree(this.builds);
    for (const row of this.rows.values()) {
      const size = await tree(this.directory(row.run));
      const reserved = activeStatuses.has(row.summary.status) || row.summary.status === "blocked" || row.run.id === reserveId;
      bytes += reserved ? Math.max(size, runReserve) : size;
    }
    return bytes;
  }
  async close() {
    if (this.closing) return;
    this.closing = true; this.timer?.(); this.timer = null;
    const active = [...this.active.values()];
    for (const value of active) value.worker.send({ type: "suspend" });
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      await Promise.race([Promise.all(active.map(value => value.worker.exited)), new Promise<void>(resolve => { timer = setTimeout(resolve, 1000); })]);
    } finally { if (timer) clearTimeout(timer); }
    for (const value of active) if (value.worker.exitCode === null) value.worker.kill("SIGKILL");
    await Promise.all(active.map(value => value.worker.exited));
    await this.queue;
    await this.catalog.close();
  }
}

async function stderrTail(stream: ReadableStream<Uint8Array>) {
  const decoder = new TextDecoder();
  let tail = "";
  for await (const chunk of stream) tail = (tail + decoder.decode(chunk, { stream: true })).slice(-4000);
  return (tail + decoder.decode()).slice(-4000);
}
